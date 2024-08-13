package contentmanager

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	"github.com/operator-framework/operator-controller/api/v1alpha1"
	oclabels "github.com/operator-framework/operator-controller/internal/labels"
	"k8s.io/client-go/dynamic/dynamicinformer"
	cgocache "k8s.io/client-go/tools/cache"
)

type Watcher interface {
	// Watch will establish watches for resources owned by a ClusterExtension
	Watch(context.Context, controller.Controller, *v1alpha1.ClusterExtension, []client.Object) error
	// Unwatch will remove watches for a ClusterExtension
	Unwatch(*v1alpha1.ClusterExtension)
}

type RestConfigMapper func(context.Context, client.Object, *rest.Config) (*rest.Config, error)

type extensionCacheData struct {
	Cache  cache.Cache
	Cancel context.CancelFunc
	GVKs   sets.Set[schema.GroupVersionKind]
}

type instance struct {
	rcm             RestConfigMapper
	baseCfg         *rest.Config
	extensionCaches map[string]extensionCacheData
	mapper          meta.RESTMapper
	mu              *sync.Mutex
	syncTimeout     time.Duration
}

// New creates a new ContentManager object
func New(rcm RestConfigMapper, cfg *rest.Config, mapper meta.RESTMapper, syncTimeout time.Duration) Watcher {
	return &instance{
		rcm:             rcm,
		baseCfg:         cfg,
		extensionCaches: make(map[string]extensionCacheData),
		mapper:          mapper,
		mu:              &sync.Mutex{},
		syncTimeout:     syncTimeout,
	}
}

// buildScheme builds a runtime.Scheme based on the provided client.Objects,
// with all GroupVersionKinds mapping to the unstructured.Unstructured type
// (unstructured.UnstructuredList for list kinds).
//
// If a provided client.Object does not set a Version or Kind field in its
// GroupVersionKind, an error will be returned.
func buildScheme(gvks []schema.GroupVersionKind) (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	// The ClusterExtension types must be added to the scheme since its
	// going to be used to establish watches that trigger reconciliation
	// of the owning ClusterExtension
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("adding operator controller APIs to scheme: %w", err)
	}

	for _, gvk := range gvks {
		listGVK := schema.GroupVersionKind{
			Group:   gvk.Group,
			Version: gvk.Version,
			Kind:    gvk.Kind + "List",
		}

		if !scheme.Recognizes(gvk) {
			// Since we can't have a mapping to every possible Go type in existence
			// based on the GVK we need to use the unstructured types for mapping
			u := &unstructured.Unstructured{}
			u.SetGroupVersionKind(gvk)
			scheme.AddKnownTypeWithName(gvk, u)

			// Adding the common meta schemas to the scheme for the GroupVersion
			// is necessary to ensure the scheme is aware of the different operations
			// that can be performed against the resources in this GroupVersion
			metav1.AddToGroupVersion(scheme, gvk.GroupVersion())
		}

		if !scheme.Recognizes(listGVK) {
			ul := &unstructured.UnstructuredList{}
			ul.SetGroupVersionKind(listGVK)
			scheme.AddKnownTypeWithName(listGVK, ul)
		}
	}

	return scheme, nil
}

func gvksForObjects(objs []client.Object) (sets.Set[schema.GroupVersionKind], error) {
	gvkSet := sets.New[schema.GroupVersionKind]()
	for _, obj := range objs {
		gvk := obj.GetObjectKind().GroupVersionKind()

		// If the Kind or Version is not set in an object's GroupVersionKind
		// attempting to add it to the runtime.Scheme will result in a panic.
		// To avoid panics, we are doing the validation and returning early
		// with an error if any objects are provided with a missing Kind or Version
		// field
		if gvk.Kind == "" {
			return nil, fmt.Errorf(
				"adding %s to set; object Kind is not defined",
				obj.GetName(),
			)
		}

		if gvk.Version == "" {
			return nil, fmt.Errorf(
				"adding %s to set; object Version is not defined",
				obj.GetName(),
			)
		}

		gvkSet.Insert(gvk)
	}

	return gvkSet, nil
}

// Watch configures a controller-runtime cache.Cache and establishes watches for the provided resources.
// It utilizes the provided ClusterExtension to set a DefaultLabelSelector on the cache.Cache
// to ensure it is only caching and reacting to content that belongs to the ClusterExtension.
// For each client.Object provided, a new source.Kind is created and used in a call to the Watch() method
// of the provided controller.Controller to establish new watches for the managed resources.
func (i *instance) Watch(ctx context.Context, ctrl controller.Controller, ce *v1alpha1.ClusterExtension, objs []client.Object) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	if len(objs) == 0 || ce == nil || ctrl == nil {
		return nil
	}

	cfg, err := i.rcm(ctx, ce, i.baseCfg)
	if err != nil {
		return fmt.Errorf("getting rest.Config for ClusterExtension %q: %w", ce.Name, err)
	}

	dynamicClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("getting dynamic client: %w", err)
	}

	// TODO: Cleanup error messages to no longer have CE name
	gvkSet, err := gvksForObjects(objs)
	if err != nil {
		return fmt.Errorf("getting set of GVKs for objects managed by ClusterExtension %q: %w", ce.Name, err)
	}

	if extensionCacheData, ok := i.extensionCaches[ce.Name]; ok {
		if gvkSet.Difference(extensionCacheData.GVKs).Len() == 0 {
			return nil
		}
	}

	scheme, err := buildScheme(gvkSet.UnsortedList())
	if err != nil {
		return fmt.Errorf("building scheme for ClusterExtension %q: %w", ce.GetName(), err)
	}

	tgtLabels := labels.Set{
		oclabels.OwnerKindKey: v1alpha1.ClusterExtensionKind,
		oclabels.OwnerNameKey: ce.GetName(),
	}

	// TODO: Instead of stopping the existing cache and replacing it every time
	// we should stop the informers that are no longer required
	// and create any new ones as necessary. To keep the initial pass
	// simple, we are going to keep this as is and optimize in a follow-up.
	// Doing this in a follow-up gives us the opportunity to verify that this functions
	// as expected when wired up in the ClusterExtension reconciler before going too deep
	// in optimizations.
	if extCache, ok := i.extensionCaches[ce.GetName()]; ok {
		extCache.Cancel()
	}

	c, err := cache.New(cfg, cache.Options{
		Scheme:               scheme,
		DefaultLabelSelector: tgtLabels.AsSelector(),
	})
	if err != nil {
		return fmt.Errorf("creating cache for ClusterExtension %q: %w", ce.Name, err)
	}

	cacheCtx, cancel := context.WithCancel(context.Background())
	go func() {
		err := c.Start(cacheCtx)
		if err != nil {
			fmt.Println("XXX DEBUG", "cache start error", err)
			i.Unwatch(ce)
		}
	}()

    // TODO: See about wrapping this lower-level informer interaction
    // into a source.SyncingSource implementation to reduce confusion
    // and bugs around cancelation of the cache context
    informerErrors := []error{}
	for _, gvk := range gvkSet.UnsortedList() {
        gvk := gvk
		sharedInfFact := dynamicinformer.NewDynamicSharedInformerFactory(dynamicClient, time.Hour*10)
		restMapping, err := i.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			cancel()
			return fmt.Errorf("getting resource mapping for GVK %q: %w", gvk, err)
		}

		gInf := sharedInfFact.ForResource(restMapping.Resource)
		sharedIndexInf := gInf.Informer()

		iec := &informerErrorCommunicator{}
		sharedIndexInf.SetWatchErrorHandler(func(r *cgocache.Reflector, err error) {
            // TODO: Look into a sync.Once to minimize structs/surface area
            iec.WriteError(err)
			cgocache.DefaultWatchErrorHandler(r, err)
		})

		go sharedIndexInf.Run(cacheCtx.Done())

		s := &source.Informer{
			Informer: sharedIndexInf,
			Handler: handler.TypedEnqueueRequestForOwner[client.Object](
				scheme,
				i.mapper,
				ce,
				handler.OnlyControllerOwner(),
			),
			Predicates: []predicate.Predicate{
				predicate.Funcs{
					CreateFunc:  func(tce event.TypedCreateEvent[client.Object]) bool { return false },
					UpdateFunc:  func(tue event.TypedUpdateEvent[client.Object]) bool { return true },
					DeleteFunc:  func(tde event.TypedDeleteEvent[client.Object]) bool { return true },
					GenericFunc: func(tge event.TypedGenericEvent[client.Object]) bool { return true },
				},
			},
		}

		err = ctrl.Watch(s)
		if err != nil {
			cancel()
			return fmt.Errorf("establishing watch for GVK %q: %w", gvk, err)
		}

		timeoutCtx, timeoutCancel := context.WithTimeout(context.TODO(), i.syncTimeout)
		defer timeoutCancel()
        err = wait.PollUntilContextCancel(timeoutCtx, time.Second, true, func(ctx context.Context) (bool, error) {
			if sharedIndexInf.HasSynced() {
				return true, nil
			}

            if iec.ReadErrors() != nil {
                return false, iec.ReadErrors()
            }

            return false, nil
		})
		if err != nil {
            informerErrors = append(informerErrors, err)
		}
	}

    if err := errors.Join(informerErrors...); err != nil {
        cancel()
        return fmt.Errorf("starting cache: %w", err)
    }

	i.extensionCaches[ce.Name] = extensionCacheData{
		Cache:  c,
		Cancel: cancel,
		GVKs:   gvkSet,
	}

	return nil
}

// TODO: Unexport this
func (i *instance) UnwatchLocked(ce *v1alpha1.ClusterExtension) {
	if ce == nil {
		return
	}

	if extCache, ok := i.extensionCaches[ce.GetName()]; ok {
		extCache.Cancel()
		delete(i.extensionCaches, ce.GetName())
	}
}

// Unwatch will stop the cache for the provided ClusterExtension
// stopping any watches on managed content
func (i *instance) Unwatch(ce *v1alpha1.ClusterExtension) {
	if ce == nil {
		return
	}

	i.mu.Lock()
	if extCache, ok := i.extensionCaches[ce.GetName()]; ok {
		extCache.Cancel()
		delete(i.extensionCaches, ce.GetName())
	}
	i.mu.Unlock()
}

type informerErrorCommunicator struct {
	mu  sync.Mutex
	err error
}

// WriteError writes the provided error to the error channel
func (iec *informerErrorCommunicator) WriteError(err error) {
	iec.mu.Lock()
	defer iec.mu.Unlock()
	if err != nil {
		iec.err = err
	}
}

func (iec *informerErrorCommunicator) ReadErrors() error {
	iec.mu.Lock()
	defer iec.mu.Unlock()
	return iec.err
}
