package contentmanager

import (
	"context"
	"errors"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	cgocache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	ocv1alpha1 "github.com/operator-framework/operator-controller/api/v1alpha1"
	oclabels "github.com/operator-framework/operator-controller/internal/labels"
)

type cacheFactory struct {
	rcm     func(context.Context, client.Object, *rest.Config) (*rest.Config, error)
	baseCfg *rest.Config
	mapper  meta.RESTMapper

	mu     sync.Mutex
	caches map[string]*extensionCache
}

func (cf *cacheFactory) deleteCache(ce *ocv1alpha1.ClusterExtension) {
	cf.mu.Lock()
	defer cf.mu.Unlock()

	if c, ok := cf.caches[ce.Name]; ok {
		c.close()
		delete(cf.caches, ce.Name)
	}
}

func (cf *cacheFactory) getOrCreateCache(ctx context.Context, ce *ocv1alpha1.ClusterExtension) (*extensionCache, error) {
	cf.mu.Lock()
	defer cf.mu.Unlock()

	if c, ok := cf.caches[ce.Name]; ok {
		return c, nil
	}

	cfg, err := cf.rcm(ctx, ce, cf.baseCfg)
	if err != nil {
		return nil, err
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	selector := labels.Set{
		oclabels.OwnerKindKey: ocv1alpha1.ClusterExtensionKind,
		oclabels.OwnerNameKey: ce.GetName(),
	}.AsSelector()

	informerFactory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(dynClient, time.Hour*10, metav1.NamespaceAll, func(options *metav1.ListOptions) {
		options.LabelSelector = selector.String()
	})

	return &extensionCache{
		ce:              ce,
		mapper:          cf.mapper,
		informerFactory: informerFactory,
	}, nil
}

type extensionCache struct {
	ce              *ocv1alpha1.ClusterExtension
	mapper          meta.RESTMapper
	informerFactory dynamicinformer.DynamicSharedInformerFactory
	controller      controller.Controller

	mu        sync.Mutex
	informers map[schema.GroupVersionKind]cacheInformer
}

func (ec *extensionCache) Get(_ context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	gvk := obj.GetObjectKind().GroupVersionKind()
	mapping, err := ec.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return err
	}

	ec.mu.Lock()
	defer ec.mu.Unlock()

	inf, ok := ec.informers[gvk]
	if !ok {
		return apierrors.NewNotFound(mapping.Resource.GroupResource(), key.Name)
	}

	runtimeObj, err := inf.lister.ByNamespace(key.Namespace).Get(key.Name)
	if err != nil {
		return err
	}
	u := runtimeObj.DeepCopyObject().(*unstructured.Unstructured)
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, obj)
}

func (ec *extensionCache) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	gvk := list.GetObjectKind().GroupVersionKind()
	mapping, err := ec.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return err
	}

	ec.mu.Lock()
	defer ec.mu.Unlock()

	inf, ok := ec.informers[gvk]
	if !ok {
		return apierrors.NewNotFound(mapping.Resource.GroupResource(), key.Name)
	}
}

type cacheInformer struct {
	informer cgocache.SharedIndexInformer
	lister   cgocache.GenericLister
	stop     chan struct{}
}

func (ec *extensionCache) close() {
	ec.mu.Lock()
	defer ec.mu.Unlock()

	for gvk, informer := range ec.informers {
		close(informer.stop)
		delete(ec.informers, gvk)
	}
}

var _ client.Reader = (*extensionCache)(nil)

func (ec *extensionCache) updateWatches(objs ...client.Object) error {
	newGVKs := objectsGVKs(objs...)
	sch := runtime.NewScheme()
	if err := ocv1alpha1.AddToScheme(sch); err != nil {
		return err
	}
	addGVKsToScheme(sch, newGVKs.UnsortedList())

	ec.mu.Lock()
	defer ec.mu.Unlock()

	currentGVKs := sets.KeySet(ec.informers)
	unwatchGVKs := currentGVKs.Difference(newGVKs)
	watchGVKs := newGVKs.Difference(currentGVKs)

	for gvk := range unwatchGVKs {
		inf, ok := ec.informers[gvk]
		if !ok {
			continue
		}
		close(inf.stop)
		delete(ec.informers, gvk)
	}

	var watchErrs []error
	for gvk := range watchGVKs {
		restMapping, err := ec.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			watchErrs = append(watchErrs, err)
			continue
		}
		resource := ec.informerFactory.ForResource(restMapping.Resource)
		inf := resource.Informer()

		var (
			stop     = make(chan struct{})
			watchErr error
		)
		_ = inf.SetWatchErrorHandler(func(_ *cgocache.Reflector, err error) {
			watchErr = err
			close(stop)
		})
		go inf.Run(stop)
		if !cgocache.WaitForCacheSync(stop, inf.HasSynced) {
			watchErrs = append(watchErrs, watchErr)
			continue
		}

		infSrc := &source.Informer{
			Informer: inf,
			Handler: handler.TypedEnqueueRequestForOwner[client.Object](
				sch,
				ec.mapper,
				ec.ce,
				handler.OnlyControllerOwner(),
			),
			Predicates: []predicate.Predicate{predicate.Funcs{
				CreateFunc:  func(tce event.TypedCreateEvent[client.Object]) bool { return false },
				UpdateFunc:  func(tue event.TypedUpdateEvent[client.Object]) bool { return true },
				DeleteFunc:  func(tde event.TypedDeleteEvent[client.Object]) bool { return true },
				GenericFunc: func(tge event.TypedGenericEvent[client.Object]) bool { return true },
			}},
		}

		if err := ec.controller.Watch(infSrc); err != nil {
			watchErrs = append(watchErrs, err)
			continue
		}

		ec.informers[gvk] = cacheInformer{
			informer: inf,
			lister:   resource.Lister(),
			stop:     stop,
		}
	}
	return errors.Join(watchErrs...)
}

func objectsGVKs(objs ...client.Object) sets.Set[schema.GroupVersionKind] {
	gvks := sets.New[schema.GroupVersionKind]()
	for _, obj := range objs {
		gvks.Insert(obj.GetObjectKind().GroupVersionKind())
	}
	return gvks
}

func addGVKsToScheme(sch *runtime.Scheme, gvks []schema.GroupVersionKind) {
	for _, gvk := range gvks {

		// Since we can't have a mapping to every possible Go type in existence
		// based on the GVK we need to use the unstructured types for mapping
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		sch.AddKnownTypeWithName(gvk, u)

		listGVK := schema.GroupVersionKind{
			Group:   gvk.Group,
			Version: gvk.Version,
			Kind:    gvk.Kind + "List",
		}
		ul := &unstructured.UnstructuredList{}
		ul.SetGroupVersionKind(listGVK)
		sch.AddKnownTypeWithName(listGVK, ul)

		// Adding the common meta schemas to the sch for the GroupVersion
		// is necessary to ensure the sch is aware of the different operations
		// that can be performed against the resources in this GroupVersion
		metav1.AddToGroupVersion(sch, gvk.GroupVersion())
	}
}
