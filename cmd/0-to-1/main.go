/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorsv1 "github.com/operator-framework/api/pkg/operators/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"

	ocv1 "github.com/operator-framework/operator-controller/api/v1"
	"github.com/operator-framework/operator-controller/internal/operator-controller/applier"
	"github.com/operator-framework/operator-controller/internal/operator-controller/labels"
)

type ref struct {
	Group     string
	Version   string
	Kind      string
	Name      string
	Namespace string
}

func main() {
	var namespaceOverride string

	rootCmd := &cobra.Command{
		Use:   "0-to-1 SUBSCRIPTION_NAME",
		Short: "Migrate OLMv0 Subscriptions to OLMv1 ClusterExtensions",
		Long: `0-to-1 is a migration tool that helps transition operators installed with
OLM v0 to OLM v1 ClusterExtensions.

This tool analyzes your existing OLMv0 installations and generates the corresponding
ClusterExtension resources for OLMv1.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			ctx := cmd.Context()
			subscriptionName := args[0]

			// Create a controller-runtime client
			restConfig, k8sClient, namespace, err := getConfigAndClient(namespaceOverride)
			if err != nil {
				return fmt.Errorf("failed to create Kubernetes client: %w", err)
			}

			m := newMigrator(restConfig, k8sClient)

			if err := m.checkOLMv0IsInstalled(); err != nil {
				return err
			}

			if err := m.initOLMv0Objects(ctx, namespace, subscriptionName); err != nil {
				return err
			}

			if err := m.initOperatorObjects(ctx); err != nil {
				return fmt.Errorf("failed to get existing objects: %w", err)
			}

			if err := m.initOLMv1Objects(ctx); err != nil {
				return err
			}

			if err := m.testPreconditions(); err != nil {
				return fmt.Errorf("Subscription %q cannot be migrated: %v", client.ObjectKeyFromObject(m.olmv0.subscription), err)
			}

			m.printInfo()

			ce := m.generateClusterExtension()
			cer, err := m.generateClusterExtensionRevision(ctx, ce)
			if err != nil {
				return fmt.Errorf("failed to generate cluster extension revision: %w", err)
			}

			if err := k8sClient.Create(ctx, cer); err != nil {
				return fmt.Errorf("failed to create cluster extension revision: %w", err)
			}

			if err := wait.PollUntilContextTimeout(ctx, time.Second, time.Second*30, true, func(ctx context.Context) (done bool, err error) {
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cer), cer); err != nil {
					return false, fmt.Errorf("failed to get ClusterExtensionRevision: %w", err)
				}
				succeeded := meta.FindStatusCondition(cer.Status.Conditions, "Succeeded")
				if succeeded == nil || succeeded.Status != metav1.ConditionTrue {
					return false, nil
				}
				return true, nil
			}); err != nil {
				return fmt.Errorf("failed to wait for cluster extension revision success: %w", err)
			}

			if err := k8sClient.Create(ctx, &ce); err != nil {
				return fmt.Errorf("failed to create ClusterExtension: %w", err)
			}

			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cer), cer); err != nil {
				return fmt.Errorf("failed to get ClusterExtension: %w", err)
			}
			ownerRef := metav1.NewControllerRef(&ce, schema.GroupVersionKind{
				Group:   ocv1.GroupVersion.Group,
				Version: ocv1.GroupVersion.Version,
				Kind:    ocv1.ClusterExtensionKind,
			})
			cer.OwnerReferences = append(cer.OwnerReferences, *ownerRef)
			if err := k8sClient.Update(ctx, cer); err != nil {
				return fmt.Errorf("failed to add owner reference to ClusterExtensionRevision: %w", err)
			}

			if err := wait.PollUntilContextTimeout(ctx, time.Second, time.Second*30, true, func(ctx context.Context) (done bool, err error) {
				if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(&ce), &ce); err != nil {
					return false, fmt.Errorf("failed to get ClusterExtension: %w", err)
				}
				installed := meta.FindStatusCondition(ce.Status.Conditions, ocv1.TypeInstalled)
				if installed == nil || installed.Status != metav1.ConditionTrue {
					return false, nil
				}
				return true, nil
			}); err != nil {
				return fmt.Errorf("failed to wait for cluster extension install completion: %w", err)
			}

			if err := wait.PollUntilContextTimeout(ctx, time.Second, time.Second*30, true, func(ctx context.Context) (done bool, err error) {
				cer2Key := types.NamespacedName{
					Name: ce.Name + "-2",
				}
				if err := k8sClient.Get(ctx, cer2Key, cer); err != nil {
					if errors.IsNotFound(err) {
						return false, nil
					}
					return false, fmt.Errorf("failed to get second ClusterExtensionRevision: %w", err)
				}
				succeeded := meta.FindStatusCondition(cer.Status.Conditions, "Succeeded")
				if succeeded == nil || succeeded.Status != metav1.ConditionTrue {
					return false, nil
				}
				return true, nil
			}); err != nil {
				return fmt.Errorf("failed to wait for second cluster extension revision success: %w", err)
			}

			if err := k8sClient.Delete(ctx, m.olmv0.subscription); err != nil {
				return fmt.Errorf("failed to delete subscription %s/%s: %w", namespace, subscriptionName, err)
			}
			if err := k8sClient.Delete(ctx, m.olmv0.clusterServiceVersion); err != nil {
				return fmt.Errorf("failed to delete ClusterServiceVersion %s/%s: %w", namespace, subscriptionName, err)
			}

			// TODO: the below patches aren't sticking. Some controller in OLMv0 is adding them back.
			//   But at this point the subscription and CSV have been deleted. We need to wait until
			//   the OLMv0 controllers' informers see the deletion events. For now, sleep, but we
			//   probably need something more robust.
			time.Sleep(2 * time.Second)

			labelKey := fmt.Sprintf("operators.coreos.com/%s.%s", m.olmv0.subscription.Spec.Package, namespace)
			labelKeyPatch := strings.ReplaceAll(labelKey, "/", "~1")
			patch := []byte(fmt.Sprintf(`[{"op":"remove", "path":"/metadata/labels/%s"}]`, labelKeyPatch))
			for _, obj := range m.objs {
				if _, ok := obj.GetLabels()[labelKey]; !ok {
					continue
				}
				fmt.Printf("Removing label %q from object %s %s/%s.\n", labelKey, obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName())
				if err := k8sClient.Patch(ctx, obj, client.RawPatch(types.JSONPatchType, patch)); err != nil && !errors.IsNotFound(err) {
					return fmt.Errorf("failed to patch %s %s/%s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetNamespace(), obj.GetName(), err)
				}
			}

			// TODO: sleeping again here to make sure the OLMv0 operator controller has reconciled to see all of the
			//   remaining exising objects be patched to be unassociated.
			time.Sleep(2 * time.Second)
			if err := k8sClient.Delete(ctx, m.olmv0.operator); err != nil {
				return fmt.Errorf("failed to delete Operator %s: %w", client.ObjectKeyFromObject(m.olmv0.operator), err)
			}

			return nil
		},
	}

	rootCmd.Flags().StringVarP(&namespaceOverride, "namespace", "n", "", "Namespace of the subscription (uses current kubeconfig context namespace if unset)")

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func getConfigAndClient(namespace string) (*rest.Config, client.Client, string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}
	if namespace != "" {
		configOverrides.Context.Namespace = namespace
	}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to get kubeconfig: %w", err)
	}

	namespace, _, err = kubeConfig.Namespace()
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to get namespace from kubeconfig: %w", err)
	}

	// Create a scheme and add OLMv0 types
	runtimeScheme := runtime.NewScheme()
	if err := scheme.AddToScheme(runtimeScheme); err != nil {
		return nil, nil, "", fmt.Errorf("failed to add k8s scheme: %w", err)
	}
	if err := operatorsv1alpha1.AddToScheme(runtimeScheme); err != nil {
		return nil, nil, "", fmt.Errorf("failed to add OLMv0 v1alpha1 scheme: %w", err)
	}
	if err := operatorsv1.AddToScheme(runtimeScheme); err != nil {
		return nil, nil, "", fmt.Errorf("failed to add OLMv0 v1 scheme: %w", err)
	}
	if err := ocv1.AddToScheme(runtimeScheme); err != nil {
		return nil, nil, "", fmt.Errorf("failed to add OLMv1 scheme: %w", err)
	}

	// Create a controller-runtime client
	cl, err := client.New(restConfig, client.Options{Scheme: runtimeScheme})
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to create client: %w", err)
	}
	return restConfig, cl, namespace, nil
}

type migrator struct {
	restConfig *rest.Config
	cl         client.Client

	olmv0 *olmv0Objects
	olmv1 *olmv1Objects
	objs  []client.Object
}

func newMigrator(restConfig *rest.Config, k8sClient client.Client) *migrator {
	return &migrator{restConfig: restConfig, cl: k8sClient}
}

type olmv0Objects struct {
	subscription          *operatorsv1alpha1.Subscription
	operatorGroups        []operatorsv1.OperatorGroup
	catalogSource         *operatorsv1alpha1.CatalogSource
	clusterServiceVersion *operatorsv1alpha1.ClusterServiceVersion
	installPlans          []operatorsv1alpha1.InstallPlan
	operator              *operatorsv1.Operator
}

type olmv1Objects struct {
	clusterExtensions         []ocv1.ClusterExtension
	clusterExtensionRevisions []ocv1.ClusterExtensionRevision
	clusterCatalogs           []ocv1.ClusterCatalog
}

func (m *migrator) checkOLMv0IsInstalled() error {
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(m.restConfig)
	if err != nil {
		return err
	}
	apiGroupList, err := discoveryClient.ServerGroups()
	if err != nil {
		return err
	}
	for _, apiGroup := range apiGroupList.Groups {
		if apiGroup.Name == operatorsv1alpha1.SchemeGroupVersion.Group {
			return nil
		}
	}
	return fmt.Errorf("OLMv0 is not installed (could not find API group %q)", operatorsv1alpha1.SchemeGroupVersion.Group)
}

func (m *migrator) initOLMv0Objects(ctx context.Context, namespace, subName string) error {
	// Get Subscription
	subscriptionKey := types.NamespacedName{Namespace: namespace, Name: subName}
	sub := &operatorsv1alpha1.Subscription{}
	if err := m.cl.Get(ctx, subscriptionKey, sub); err != nil {
		return fmt.Errorf("failed to get Subscription %s: %w", subscriptionKey, err)
	}

	// Get OperatorGroup
	ogList := &operatorsv1.OperatorGroupList{}
	if err := m.cl.List(ctx, ogList, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("failed to get OperatorGroup list in namespace %s: %w", subscriptionKey, err)
	}

	// Get CatalogSource
	catSrcKey := types.NamespacedName{Namespace: sub.Spec.CatalogSourceNamespace, Name: sub.Spec.CatalogSource}
	catSrc := &operatorsv1alpha1.CatalogSource{}
	if err := m.cl.Get(ctx, catSrcKey, catSrc); err != nil {
		return fmt.Errorf("failed to get CatalogSource %s: %w", catSrcKey, err)
	}

	// Get CSV and InstallPlans
	var csv *operatorsv1alpha1.ClusterServiceVersion
	var ips []operatorsv1alpha1.InstallPlan
	if sub.Status.InstalledCSV != "" {
		// Get CSV
		csvKey := types.NamespacedName{Namespace: namespace, Name: sub.Status.InstalledCSV}
		csv = &operatorsv1alpha1.ClusterServiceVersion{}
		if err := m.cl.Get(ctx, csvKey, csv); err != nil {
			return fmt.Errorf("failed to get ClusterServiceVersion %s: %w", csvKey, err)
		}

		// Get InstallPlans
		ipList := &operatorsv1alpha1.InstallPlanList{}
		if err := m.cl.List(ctx, ipList, client.InNamespace(namespace)); err != nil {
			return fmt.Errorf("failed to get InstallPlan list in namespace %s: %w", namespace, err)
		}
		ips = slices.DeleteFunc(ipList.Items, func(ip operatorsv1alpha1.InstallPlan) bool {
			for _, bl := range ip.Status.BundleLookups {
				if bl.Identifier == sub.Status.InstalledCSV {
					return false
				}
			}
			return true
		})
		slices.SortFunc(ips, func(a, b operatorsv1alpha1.InstallPlan) int {
			return cmp.Compare(a.Spec.Generation, b.Spec.Generation)
		})
	}

	// Get Operator
	opKey := types.NamespacedName{Name: fmt.Sprintf("%s.%s", sub.Spec.Package, sub.Namespace)}
	op := &operatorsv1.Operator{}
	if err := m.cl.Get(ctx, opKey, op); err != nil {
		return fmt.Errorf("failed to get Operator %s: %w", opKey.Name, err)
	}

	m.olmv0 = &olmv0Objects{
		subscription:          sub,
		operatorGroups:        ogList.Items,
		catalogSource:         catSrc,
		clusterServiceVersion: csv,
		installPlans:          ips,
		operator:              op,
	}
	return nil
}

func (m *migrator) initOLMv1Objects(ctx context.Context) error {
	// List ClusterExtensions with same package name
	ceList := &ocv1.ClusterExtensionList{}
	if err := m.cl.List(ctx, ceList); err != nil {
		return fmt.Errorf("failed to get ClusterExtension list: %w", err)
	}
	ces := slices.DeleteFunc(ceList.Items, func(item ocv1.ClusterExtension) bool {
		return item.Spec.Source.Catalog == nil || item.Spec.Source.Catalog.PackageName != m.olmv0.subscription.Spec.Package
	})

	// List ClusterExtensionRevisions with same package name
	cerList := &ocv1.ClusterExtensionRevisionList{}
	if err := m.cl.List(ctx, cerList); err != nil {
		return fmt.Errorf("failed to get ClusterExtensionRevision list: %w", err)
	}
	cers := slices.DeleteFunc(cerList.Items, func(item ocv1.ClusterExtensionRevision) bool {
		return item.Annotations[labels.PackageNameKey] != m.olmv0.subscription.Spec.Package
	})

	// List ClusterCatalogs
	ccList := &ocv1.ClusterCatalogList{}
	if err := m.cl.List(ctx, ccList); err != nil {
		return fmt.Errorf("failed to get ClusterCatalog list: %w", err)
	}

	m.olmv1 = &olmv1Objects{
		clusterExtensions:         ces,
		clusterExtensionRevisions: cers,
		clusterCatalogs:           ccList.Items,
	}
	return nil
}

func (m *migrator) testPreconditions() error {
	preconditions := []precondition{
		m.exactlyOneOperatorGroup,
		m.allNamespacesOperatorGroup,
		m.grpcImageCatalogSource,
		m.foundInstalledCSV,
		m.installPlansAreComplete,
		m.noClusterExtensionsWithSamePackage,
		m.noClusterExtensionRevisionsWithSamePackage,
		m.hasClusterCatalogWithSameImage,
	}
	for _, p := range preconditions {
		if err := p(); err != nil {
			return err
		}
	}
	return nil
}

func (m *migrator) exactlyOneOperatorGroup() error {
	return simplePrecondition(
		len(m.olmv0.operatorGroups) == 1,
		"expected exactly one OperatorGroup, got %d",
		len(m.olmv0.operatorGroups),
	)
}

func (m *migrator) allNamespacesOperatorGroup() error {
	og := m.olmv0.operatorGroups[0]
	installMode := getInstallMode(&og)
	if installMode == "MultiNamespace" {
		return fmt.Errorf("MultiNamespace install mode is not supported in OLM v1")
	}
	if installMode != "AllNamespaces" {
		// TODO: we could do a few things here:
		//   1. Require user acknowledgement that OLMv1 has limited support for OLMv0's single and own namespace install modes
		//   2. Check that no other subscriptions exist in the cluster for this same package.
		//   3. Allow override to allow migration to continue anyway.
		return fmt.Errorf("OperatorGroup must target all namespaces")
	}
	return nil
}

func (m *migrator) grpcImageCatalogSource() error {
	catSrc := m.olmv0.catalogSource
	return simplePrecondition(
		catSrc.Spec.SourceType == operatorsv1alpha1.SourceTypeGrpc && catSrc.Spec.Image != "",
		"CatalogSource %q must specify an image with source type grpc", client.ObjectKeyFromObject(catSrc),
	)
}

func (m *migrator) foundInstalledCSV() error {
	return simplePrecondition(
		m.olmv0.clusterServiceVersion != nil,
		"ClusterServiceVersion must be installed",
	)
}

func (m *migrator) installPlansAreComplete() error {
	return simplePrecondition(
		!slices.ContainsFunc(m.olmv0.installPlans, func(ip operatorsv1alpha1.InstallPlan) bool {
			return ip.Status.Phase != operatorsv1alpha1.InstallPlanPhaseComplete
		}),
		"All InstallPlans for installed CSV must be complete",
	)
}

func (m *migrator) noClusterExtensionsWithSamePackage() error {
	ceNames := make([]string, 0, len(m.olmv1.clusterExtensions))
	for _, ce := range m.olmv1.clusterExtensions {
		ceNames = append(ceNames, strconv.Quote(ce.Name))
	}
	return simplePrecondition(
		len(m.olmv1.clusterExtensions) == 0,
		"No ClusterExtension can already be present for the same package name, found %s",
		strings.Join(ceNames, ","),
	)
}
func (m *migrator) noClusterExtensionRevisionsWithSamePackage() error {
	cerNames := make([]string, 0, len(m.olmv1.clusterExtensionRevisions))
	for _, ce := range m.olmv1.clusterExtensionRevisions {
		cerNames = append(cerNames, strconv.Quote(ce.Name))
	}
	return simplePrecondition(
		len(m.olmv1.clusterExtensionRevisions) == 0,
		"No ClusterExtensionRevision can already be present for the same package name, found %s",
		strings.Join(cerNames, ","),
	)
}
func (m *migrator) hasClusterCatalogWithSameImage() error {
	cc := m.findMatchingClusterCatalog()
	if cc == nil {
		return fmt.Errorf("No ClusterCatalog exists that matches the catalog source's image reference %q", m.olmv0.catalogSource.Spec.Image)
	}
	return nil
}

func (m *migrator) findMatchingClusterCatalog() *ocv1.ClusterCatalog {
	expectCatalogImage := m.olmv0.catalogSource.Spec.Image
	for _, cc := range m.olmv1.clusterCatalogs {
		if cc.Spec.Source.Image != nil && cc.Spec.Source.Image.Ref == expectCatalogImage {
			return &cc
		}
	}
	return nil
}

func simplePrecondition(condition bool, message string, args ...interface{}) error {
	if !condition {
		return fmt.Errorf(message, args...)
	}
	return nil
}

type precondition func() error

func (m *migrator) initOperatorObjects(ctx context.Context) error {
	namespace := m.olmv0.subscription.Namespace
	ipResources := sets.New[ref]()
	// TODO: we shouldn't have to pull steps from the install plan, because the "operators" API should
	//   have a listing of all objects associated with an operator's installation. But it seems like
	//   bundle objects that are directly included in the bundle's /manifests directory don't end up with
	//   the operators.coreos.com/<packageName>.<namespace> label. That seems like a bug in OLMv0 we should
	//   look into and fix.
	for _, step := range m.olmv0.installPlans[0].Status.Plan {
		if step.Resolving == m.olmv0.subscription.Status.InstalledCSV {
			ipResources.Insert(ref{
				Group:     step.Resource.Group,
				Version:   step.Resource.Version,
				Kind:      step.Resource.Kind,
				Name:      step.Resource.Name,
				Namespace: namespace,
			})
		}
	}
	for _, componentRef := range m.olmv0.operator.Status.Components.Refs {
		gvk := schema.FromAPIVersionAndKind(componentRef.APIVersion, componentRef.Kind)
		ns := componentRef.Namespace
		if ns == "" {
			ns = namespace
		}
		ipResources.Insert(ref{
			Group:     gvk.Group,
			Version:   gvk.Version,
			Kind:      gvk.Kind,
			Name:      componentRef.Name,
			Namespace: ns,
		})
	}

	ipResourcesList := slices.SortedFunc(maps.Keys(ipResources), func(a, b ref) int {
		return cmp.Or(
			cmp.Compare(a.Group, b.Group),
			cmp.Compare(a.Version, b.Version),
			cmp.Compare(a.Kind, b.Kind),
			cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(a.Name, b.Name),
		)
	})

	objs := make([]client.Object, 0, len(ipResourcesList))
	for _, ref := range ipResourcesList {
		gvk := schema.GroupVersionKind{Group: ref.Group, Version: ref.Version, Kind: ref.Kind}
		if gvk.Group == operatorsv1.GroupVersion.Group {
			continue
		}
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(gvk)
		if err := m.cl.Get(ctx, client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}, u); err != nil {
			return err
		}
		objs = append(objs, u)
	}
	m.objs = objs
	return nil
}

func (m *migrator) findBundleImageReference() string {
	for _, bl := range m.olmv0.installPlans[0].Status.BundleLookups {
		if bl.Identifier == m.olmv0.clusterServiceVersion.Name {
			return bl.Path
		}
	}
	return ""
}

func (m *migrator) printInfo() {
	packageName := m.olmv0.subscription.Spec.Package
	channel := m.olmv0.subscription.Spec.Channel
	installedVersion := m.olmv0.clusterServiceVersion.Spec.Version.String()

	// Display the gathered information
	fmt.Printf("CatalogSource: %q\n", client.ObjectKeyFromObject(m.olmv0.catalogSource))
	fmt.Printf("Matched ClusterCatalog: %q\n", client.ObjectKeyFromObject(m.findMatchingClusterCatalog()))
	fmt.Printf("Subscription: %q\n", client.ObjectKeyFromObject(m.olmv0.subscription))
	fmt.Printf("Bundle Image Reference: %s\n", m.findBundleImageReference())
	fmt.Printf("CSV: %q\n", client.ObjectKeyFromObject(m.olmv0.clusterServiceVersion))
	fmt.Printf("Package: %q\n", packageName)
	fmt.Printf("Channel: %q\n", channel)
	fmt.Printf("Version: %q\n", installedVersion)
	fmt.Printf("Install Mode: %q\n", getInstallMode(&m.olmv0.operatorGroups[0]))
	fmt.Printf("Approval: %q\n", m.olmv0.subscription.Spec.InstallPlanApproval)

	fmt.Println("Existing components:")

	for _, obj := range m.objs {
		nn := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}.String()
		if obj.GetNamespace() == "" {
			nn = obj.GetName()
		}
		gvk := obj.GetObjectKind().GroupVersionKind()
		fmt.Printf("  %s %s %s\n", gvk.Kind, gvk.GroupVersion(), nn)
	}
}

func getInstallMode(og *operatorsv1.OperatorGroup) string {
	targetNamespaces := og.Status.Namespaces
	installMode := ""
	if len(targetNamespaces) == 0 || (len(targetNamespaces) == 1 && targetNamespaces[0] == corev1.NamespaceAll) {
		installMode = "AllNamespaces"
	} else if len(targetNamespaces) == 1 && targetNamespaces[0] == og.Namespace {
		installMode = "OwnNamespace"
	} else if len(targetNamespaces) == 1 && targetNamespaces[0] != og.Namespace {
		installMode = "SingleNamespace"
	} else {
		installMode = "MultiNamespace"
	}
	return installMode
}

func (m *migrator) generateClusterExtension() ocv1.ClusterExtension {
	channel := m.olmv0.subscription.Spec.Channel
	channels := make([]string, 0, 1)
	if channel != "" {
		channels = append(channels, channel)
	}
	return ocv1.ClusterExtension{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ClusterExtension",
			APIVersion: ocv1.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: m.olmv0.subscription.Name,
			Annotations: map[string]string{
				"olm.operatorframework.io/migrated-from-v0": "true",
			},
		},
		Spec: ocv1.ClusterExtensionSpec{
			Namespace: m.olmv0.subscription.Namespace,
			ServiceAccount: ocv1.ServiceAccountReference{
				Name: fmt.Sprintf("%s-installer", m.olmv0.subscription.Spec.Package),
			},
			Source: ocv1.SourceConfig{
				SourceType: ocv1.SourceTypeCatalog,
				Catalog: &ocv1.CatalogFilter{
					PackageName: m.olmv0.subscription.Spec.Package,
					Channels:    channels,
					Version:     m.olmv0.clusterServiceVersion.Spec.Version.String(),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"olm.operatorframework.io/metadata.name": m.findMatchingClusterCatalog().Name,
						},
					},
				},
			},
		},
	}
}

func (m *migrator) generateClusterExtensionRevision(ctx context.Context, ext ocv1.ClusterExtension) (*ocv1.ClusterExtensionRevision, error) {
	revGenerator := applier.SimpleRevisionGenerator{}
	cer, err := revGenerator.GenerateRevisionFromObjects(ctx, &ext, ocv1.CollisionProtectionNone, m.objs, nil, map[string]string{
		"olm.operatorframework.io/migrated-from-v0": "true",
		labels.PackageNameKey:                       m.olmv0.subscription.Spec.Package,
		labels.BundleNameKey:                        m.olmv0.clusterServiceVersion.Name,
		labels.BundleVersionKey:                     m.olmv0.clusterServiceVersion.Spec.Version.String(),
		labels.BundleReferenceKey:                   m.findBundleImageReference(),
	})
	if err != nil {
		return nil, err
	}
	cer.SetName(fmt.Sprintf("%s-1", ext.Name))
	cer.Spec.Revision = 1
	return cer, nil
}
