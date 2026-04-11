//go:build !standard

package controllers

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/clock"
	"pkg.package-operator.run/boxcutter"
	"pkg.package-operator.run/boxcutter/machinery"
	machinerytypes "pkg.package-operator.run/boxcutter/machinery/types"
	"pkg.package-operator.run/boxcutter/ownerhandling"
	"pkg.package-operator.run/boxcutter/probing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	ocv1 "github.com/operator-framework/operator-controller/api/v1"
	"github.com/operator-framework/operator-controller/internal/operator-controller/labels"
)

const (
	clusterObjectSetTeardownFinalizer = "olm.operatorframework.io/teardown"
)

// ClusterObjectSetReconciler actions individual snapshots of ClusterExtensions,
// as part of the boxcutter integration.
type ClusterObjectSetReconciler struct {
	Client                client.Client
	RevisionEngineFactory RevisionEngineFactory
	TrackingCache         trackingCache
	Clock                 clock.Clock
}

type trackingCache interface {
	client.Reader
	Source(handler handler.EventHandler, predicates ...predicate.Predicate) source.Source
	Watch(ctx context.Context, user client.Object, gvks sets.Set[schema.GroupVersionKind]) error
	Free(ctx context.Context, user client.Object) error
}

//+kubebuilder:rbac:groups=olm.operatorframework.io,resources=clusterobjectsets,verbs=get;list;watch;update;patch;create;delete
//+kubebuilder:rbac:groups=olm.operatorframework.io,resources=clusterobjectsets/status,verbs=update;patch
//+kubebuilder:rbac:groups=olm.operatorframework.io,resources=clusterobjectsets/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (c *ClusterObjectSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx).WithName("cluster-extension-revision")
	ctx = log.IntoContext(ctx, l)

	existingRev := &ocv1.ClusterObjectSet{}
	if err := c.Client.Get(ctx, req.NamespacedName, existingRev); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	l.Info("reconcile starting")
	defer l.Info("reconcile ending")

	reconciledRev := existingRev.DeepCopy()
	res, reconcileErr := c.reconcile(ctx, reconciledRev)

	// Do checks before any Update()s, as Update() may modify the resource structure!
	updateStatus := !equality.Semantic.DeepEqual(existingRev.Status, reconciledRev.Status)

	unexpectedFieldsChanged := checkForUnexpectedClusterObjectSetFieldChange(*existingRev, *reconciledRev)
	if unexpectedFieldsChanged {
		panic("spec or metadata changed by reconciler")
	}

	// NOTE: finalizer updates are performed during c.reconcile as patches, so that reconcile can
	//   continue performing logic after successfully setting the finalizer. therefore we only need
	//   to set status here.

	if updateStatus {
		if err := c.Client.Status().Update(ctx, reconciledRev); err != nil {
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("error updating status: %v", err))
		}
	}

	return res, reconcileErr
}

// Compare resources - ignoring status & metadata.finalizers
func checkForUnexpectedClusterObjectSetFieldChange(a, b ocv1.ClusterObjectSet) bool {
	a.Status, b.Status = ocv1.ClusterObjectSetStatus{}, ocv1.ClusterObjectSetStatus{}

	// when finalizers are updated during reconcile, we expect finalizers, managedFields, and resourceVersion
	// to be updated, so we ignore changes in these fields.
	a.Finalizers, b.Finalizers = []string{}, []string{}
	a.ManagedFields, b.ManagedFields = nil, nil
	a.ResourceVersion, b.ResourceVersion = "", ""
	return !equality.Semantic.DeepEqual(a.Spec, b.Spec)
}

func (c *ClusterObjectSetReconciler) reconcile(ctx context.Context, cos *ocv1.ClusterObjectSet) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	if !cos.DeletionTimestamp.IsZero() {
		return c.delete(ctx, cos)
	}

	remaining, hasDeadline := c.durationUntilDeadline(cos)
	isDeadlineExceeded := hasDeadline && remaining <= 0

	phases, opts, err := c.buildBoxcutterPhases(ctx, cos)
	if err != nil {
		setRetryingConditions(cos, err.Error(), isDeadlineExceeded)
		return ctrl.Result{}, fmt.Errorf("converting to boxcutter revision: %v", err)
	}

	revisionEngine, err := c.RevisionEngineFactory.CreateRevisionEngine(ctx, cos)
	if err != nil {
		setRetryingConditions(cos, err.Error(), isDeadlineExceeded)
		return ctrl.Result{}, fmt.Errorf("failed to create revision engine: %v", err)
	}

	revision := boxcutter.NewRevisionWithOwner(
		cos.Name,
		cos.Spec.Revision,
		phases,
		cos,
		ownerhandling.NewNative(c.Client.Scheme()),
	)

	if cos.Spec.LifecycleState == ocv1.ClusterObjectSetLifecycleStateArchived {
		if err := c.TrackingCache.Free(ctx, cos); err != nil {
			markAsAvailableUnknown(cos, ocv1.ClusterObjectSetReasonReconciling, err.Error())
			return ctrl.Result{}, fmt.Errorf("error stopping informers: %v", err)
		}
		return c.archive(ctx, revisionEngine, cos, revision)
	}

	if err := c.ensureFinalizer(ctx, cos, clusterObjectSetTeardownFinalizer); err != nil {
		return ctrl.Result{}, fmt.Errorf("error ensuring teardown finalizer: %v", err)
	}

	if err := c.establishWatch(ctx, cos, revision); err != nil {
		werr := fmt.Errorf("establish watch: %v", err)
		setRetryingConditions(cos, werr.Error(), isDeadlineExceeded)
		return ctrl.Result{}, werr
	}

	rres, err := revisionEngine.Reconcile(ctx, revision, opts...)
	if err != nil {
		if rres != nil {
			// Log detailed reconcile reports only in debug mode (V(1)) to reduce verbosity.
			l.V(1).Info("reconcile report", "report", rres.String())
		}
		setRetryingConditions(cos, err.Error(), isDeadlineExceeded)
		return ctrl.Result{}, fmt.Errorf("revision reconcile: %v", err)
	}

	// Retry failing preflight checks with a flat 10s retry.
	// TODO: report status, backoff?
	if verr := rres.GetValidationError(); verr != nil {
		l.Error(fmt.Errorf("%w", verr), "preflight validation failed, retrying after 10s")
		setRetryingConditions(cos, fmt.Sprintf("revision validation error: %s", verr), isDeadlineExceeded)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	for i, pres := range rres.GetPhases() {
		if verr := pres.GetValidationError(); verr != nil {
			l.Error(fmt.Errorf("%w", verr), "phase preflight validation failed, retrying after 10s", "phase", i)
			setRetryingConditions(cos, fmt.Sprintf("phase %d validation error: %s", i, verr), isDeadlineExceeded)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		var collidingObjs []string
		for _, ores := range pres.GetObjects() {
			if ores.Action() == machinery.ActionCollision {
				collidingObjs = append(collidingObjs, ores.String())
			}
		}

		if len(collidingObjs) > 0 {
			l.Error(fmt.Errorf("object collision detected"), "object collision, retrying after 10s", "phase", i, "collisions", collidingObjs)
			setRetryingConditions(cos, fmt.Sprintf("revision object collisions in phase %d\n%s", i, strings.Join(collidingObjs, "\n\n")), isDeadlineExceeded)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}

	revVersion := cos.GetAnnotations()[labels.BundleVersionKey]
	if rres.InTransition() {
		markAsProgressing(cos, ocv1.ReasonRollingOut, fmt.Sprintf("Revision %s is rolling out.", revVersion), isDeadlineExceeded)
	}

	//nolint:nestif
	if rres.IsComplete() {
		// Archive previous revisions
		previous, err := c.listPreviousRevisions(ctx, cos)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("listing previous revisions: %v", err)
		}
		for _, a := range previous {
			patch := []byte(`{"spec":{"lifecycleState":"Archived"}}`)
			if err := c.Client.Patch(ctx, client.Object(a), client.RawPatch(types.MergePatchType, patch)); err != nil {
				// TODO: It feels like an error here needs to propagate to a status _somewhere_.
				//   Not sure the current COS makes sense? But it also feels off to set the CE
				//   status from outside the CE reconciler.
				return ctrl.Result{}, fmt.Errorf("archive previous Revision: %w", err)
			}
		}

		markAsProgressing(cos, ocv1.ReasonSucceeded, fmt.Sprintf("Revision %s has rolled out.", revVersion), isDeadlineExceeded)
		markAsAvailable(cos, ocv1.ClusterObjectSetReasonProbesSucceeded, "Objects are available and pass all probes.")

		// We'll probably only want to remove this once we are done updating the ClusterExtension conditions
		// as its one of the interfaces between the revision and the extension. If we still have the Succeeded for now
		// that's fine.
		meta.SetStatusCondition(&cos.Status.Conditions, metav1.Condition{
			Type:               ocv1.ClusterObjectSetTypeSucceeded,
			Status:             metav1.ConditionTrue,
			Reason:             ocv1.ReasonSucceeded,
			Message:            "Revision succeeded rolling out.",
			ObservedGeneration: cos.Generation,
		})
	} else {
		var probeFailureMsgs []string
		for _, pres := range rres.GetPhases() {
			if pres.IsComplete() {
				continue
			}
			for _, ores := range pres.GetObjects() {
				// we probably want an AvailabilityProbeType and run through all of them independently of whether
				// the revision is complete or not
				pr := ores.ProbeResults()[boxcutter.ProgressProbeType]
				if pr.Status == machinerytypes.ProbeStatusTrue {
					continue
				}

				obj := ores.Object()
				gvk := obj.GetObjectKind().GroupVersionKind()
				// I think these can be pretty large and verbose. We may want to
				// work a little on the formatting...?
				probeFailureMsgs = append(probeFailureMsgs, fmt.Sprintf(
					"Object %s.%s %s/%s: %v",
					gvk.Kind, gvk.GroupVersion().String(),
					obj.GetNamespace(), obj.GetName(), strings.Join(pr.Messages, " and "),
				))
				break
			}
		}

		if len(probeFailureMsgs) > 0 {
			markAsUnavailable(cos, ocv1.ClusterObjectSetReasonProbeFailure, strings.Join(probeFailureMsgs, "\n"))
		} else {
			markAsUnavailable(cos, ocv1.ReasonRollingOut, fmt.Sprintf("Revision %s is rolling out.", revVersion))
		}
		markAsProgressing(cos, ocv1.ReasonRollingOut, fmt.Sprintf("Revision %s is rolling out.", revVersion), isDeadlineExceeded)
		return c.requeueForDeadline(cos), nil
	}

	return ctrl.Result{}, nil
}

// requeueForDeadline returns a Result that requeues at the progress deadline
// if one is configured and has not yet been exceeded. This ensures that
// ProgressDeadlineExceeded is set promptly even when no object events occur.
func (c *ClusterObjectSetReconciler) requeueForDeadline(cos *ocv1.ClusterObjectSet) ctrl.Result {
	if remaining, hasDeadline := c.durationUntilDeadline(cos); hasDeadline && remaining > 0 {
		return ctrl.Result{RequeueAfter: remaining}
	}
	return ctrl.Result{}
}

func (c *ClusterObjectSetReconciler) delete(ctx context.Context, cos *ocv1.ClusterObjectSet) (ctrl.Result, error) {
	if err := c.TrackingCache.Free(ctx, cos); err != nil {
		markAsAvailableUnknown(cos, ocv1.ClusterObjectSetReasonReconciling, err.Error())
		return ctrl.Result{}, fmt.Errorf("error stopping informers: %v", err)
	}
	if err := c.removeFinalizer(ctx, cos, clusterObjectSetTeardownFinalizer); err != nil {
		return ctrl.Result{}, fmt.Errorf("error removing teardown finalizer: %v", err)
	}
	return ctrl.Result{}, nil
}

func (c *ClusterObjectSetReconciler) archive(ctx context.Context, revisionEngine RevisionEngine, cos *ocv1.ClusterObjectSet, revision boxcutter.RevisionBuilder) (ctrl.Result, error) {
	tdres, err := revisionEngine.Teardown(ctx, revision)
	if err != nil {
		err = fmt.Errorf("error archiving revision: %v", err)
		setRetryingConditions(cos, err.Error(), false)
		return ctrl.Result{}, err
	}
	if tdres != nil && !tdres.IsComplete() {
		setRetryingConditions(cos, "removing revision resources that are not owned by another revision", false)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	// Ensure conditions are set before removing the finalizer when archiving
	if markAsArchived(cos) {
		return ctrl.Result{}, nil
	}
	if err := c.removeFinalizer(ctx, cos, clusterObjectSetTeardownFinalizer); err != nil {
		return ctrl.Result{}, fmt.Errorf("error removing teardown finalizer: %v", err)
	}
	return ctrl.Result{}, nil
}

type Sourcoser interface {
	Source(handler handler.EventHandler, predicates ...predicate.Predicate) source.Source
}

func (c *ClusterObjectSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	c.Clock = clock.RealClock{}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(controller.Options{
			RateLimiter: newDeadlineAwareRateLimiter(
				workqueue.DefaultTypedControllerRateLimiter[ctrl.Request](),
				mgr.GetClient(),
				c.Clock,
			),
		}).
		For(
			&ocv1.ClusterObjectSet{},
			builder.WithPredicates(
				predicate.ResourceVersionChangedPredicate{},
			),
		).
		WatchesRawSource(
			c.TrackingCache.Source(
				handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(), &ocv1.ClusterObjectSet{}),
				predicate.ResourceVersionChangedPredicate{},
			),
		).
		Complete(c)
}

// deadlineAwareRateLimiter wraps a delegate rate limiter and caps the backoff
// duration to the time remaining until the COS progress deadline (+2s), ensuring
// that ProgressDeadlineExceeded is set promptly even during exponential backoff.
type deadlineAwareRateLimiter struct {
	delegate workqueue.TypedRateLimiter[ctrl.Request]
	client   client.Reader
	clock    clock.Clock
}

func newDeadlineAwareRateLimiter(
	delegate workqueue.TypedRateLimiter[ctrl.Request],
	c client.Reader,
	clk clock.Clock,
) *deadlineAwareRateLimiter {
	return &deadlineAwareRateLimiter{delegate: delegate, client: c, clock: clk}
}

func (r *deadlineAwareRateLimiter) When(item ctrl.Request) time.Duration {
	backoff := r.delegate.When(item)

	cos := &ocv1.ClusterObjectSet{}
	if err := r.client.Get(context.Background(), item.NamespacedName, cos); err != nil {
		return backoff
	}

	remaining, hasDeadline := durationUntilDeadline(r.clock, cos)
	if !hasDeadline {
		return backoff
	}

	deadline := remaining + 2*time.Second
	if deadline > 0 && deadline < backoff {
		return deadline
	}
	return backoff
}

func (r *deadlineAwareRateLimiter) Forget(item ctrl.Request) {
	r.delegate.Forget(item)
}

func (r *deadlineAwareRateLimiter) NumRequeues(item ctrl.Request) int {
	return r.delegate.NumRequeues(item)
}

func (c *ClusterObjectSetReconciler) establishWatch(ctx context.Context, cos *ocv1.ClusterObjectSet, revision boxcutter.RevisionBuilder) error {
	gvks := sets.New[schema.GroupVersionKind]()
	for _, phase := range revision.GetPhases() {
		for _, obj := range phase.GetObjects() {
			gvks.Insert(obj.GetObjectKind().GroupVersionKind())
		}
	}

	return c.TrackingCache.Watch(ctx, cos, gvks)
}

func (c *ClusterObjectSetReconciler) ensureFinalizer(
	ctx context.Context, obj client.Object, finalizer string,
) error {
	if controllerutil.ContainsFinalizer(obj, finalizer) {
		return nil
	}

	controllerutil.AddFinalizer(obj, finalizer)
	patch := map[string]any{
		"metadata": map[string]any{
			"resourceVersion": obj.GetResourceVersion(),
			"finalizers":      obj.GetFinalizers(),
		},
	}
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshalling patch to remove finalizer: %w", err)
	}
	if err := c.Client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patchJSON)); err != nil {
		return fmt.Errorf("adding finalizer: %w", err)
	}
	return nil
}

func (c *ClusterObjectSetReconciler) removeFinalizer(ctx context.Context, obj client.Object, finalizer string) error {
	if !controllerutil.ContainsFinalizer(obj, finalizer) {
		return nil
	}

	controllerutil.RemoveFinalizer(obj, finalizer)

	patch := map[string]any{
		"metadata": map[string]any{
			"resourceVersion": obj.GetResourceVersion(),
			"finalizers":      obj.GetFinalizers(),
		},
	}
	patchJSON, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshalling patch to remove finalizer: %w", err)
	}
	if err := c.Client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patchJSON)); err != nil {
		return fmt.Errorf("removing finalizer: %w", err)
	}
	return nil
}

// listPreviousRevisions returns active revisions belonging to the same ClusterExtension with lower revision numbers.
// Filters out the current revision, archived revisions, deleting revisions, and revisions with equal or higher numbers.
func (c *ClusterObjectSetReconciler) listPreviousRevisions(ctx context.Context, cos *ocv1.ClusterObjectSet) ([]*ocv1.ClusterObjectSet, error) {
	ownerLabel, ok := cos.Labels[labels.OwnerNameKey]
	if !ok {
		// No owner label means this revision isn't properly labeled - return empty list
		return nil, nil
	}

	revList := &ocv1.ClusterObjectSetList{}
	if err := c.TrackingCache.List(ctx, revList, client.MatchingLabels{
		labels.OwnerNameKey: ownerLabel,
	}); err != nil {
		return nil, fmt.Errorf("listing revisions: %w", err)
	}

	previous := make([]*ocv1.ClusterObjectSet, 0, len(revList.Items))
	for i := range revList.Items {
		r := &revList.Items[i]
		if r.Name == cos.Name {
			continue
		}
		// Skip archived or deleting revisions
		if r.Spec.LifecycleState == ocv1.ClusterObjectSetLifecycleStateArchived ||
			!r.DeletionTimestamp.IsZero() {
			continue
		}
		// Only include revisions with lower revision numbers (actual previous revisions)
		if r.Spec.Revision >= cos.Spec.Revision {
			continue
		}
		previous = append(previous, r)
	}

	return previous, nil
}

func (c *ClusterObjectSetReconciler) buildBoxcutterPhases(ctx context.Context, cos *ocv1.ClusterObjectSet) ([]boxcutter.Phase, []boxcutter.RevisionReconcileOption, error) {
	previous, err := c.listPreviousRevisions(ctx, cos)
	if err != nil {
		return nil, nil, fmt.Errorf("listing previous revisions: %w", err)
	}

	// Convert to []client.Object for boxcutter
	previousObjs := make([]client.Object, len(previous))
	for i, rev := range previous {
		previousObjs[i] = rev
	}

	progressionProbes, err := buildProgressionProbes(cos.Spec.ProgressionProbes)
	if err != nil {
		return nil, nil, err
	}

	opts := []boxcutter.RevisionReconcileOption{
		boxcutter.WithPreviousOwners(previousObjs),
		boxcutter.WithProbe(boxcutter.ProgressProbeType, progressionProbes),
		boxcutter.WithAggregatePhaseReconcileErrors(),
	}

	phases := make([]boxcutter.Phase, 0)
	for _, specPhase := range cos.Spec.Phases {
		objs := make([]client.Object, 0)
		for _, specObj := range specPhase.Objects {
			var obj *unstructured.Unstructured
			switch {
			case specObj.Object.Object != nil:
				obj = specObj.Object.DeepCopy()
			case specObj.Ref.Name != "":
				resolved, err := c.resolveObjectRef(ctx, specObj.Ref)
				if err != nil {
					return nil, nil, fmt.Errorf("resolving ref in phase %q: %w", specPhase.Name, err)
				}
				obj = resolved
			default:
				return nil, nil, fmt.Errorf("object in phase %q has neither object nor ref", specPhase.Name)
			}

			objLabels := obj.GetLabels()
			if objLabels == nil {
				objLabels = map[string]string{}
			}
			objLabels[labels.OwnerNameKey] = cos.Labels[labels.OwnerNameKey]
			obj.SetLabels(objLabels)

			switch cp := EffectiveCollisionProtection(cos.Spec.CollisionProtection, specPhase.CollisionProtection, specObj.CollisionProtection); cp {
			case ocv1.CollisionProtectionIfNoController, ocv1.CollisionProtectionNone:
				opts = append(opts, boxcutter.WithObjectReconcileOptions(
					obj, boxcutter.WithCollisionProtection(cp)))
			}

			objs = append(objs, obj)
		}
		phases = append(phases, boxcutter.NewPhase(specPhase.Name, objs))
	}
	return phases, opts, nil
}

// resolveObjectRef fetches the referenced Secret, reads the value at the specified key,
// auto-detects gzip compression, and deserializes into an unstructured.Unstructured.
func (c *ClusterObjectSetReconciler) resolveObjectRef(ctx context.Context, ref ocv1.ObjectSourceRef) (*unstructured.Unstructured, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}
	if err := c.Client.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("getting Secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	data, ok := secret.Data[ref.Key]
	if !ok {
		return nil, fmt.Errorf("key %q not found in Secret %s/%s", ref.Key, ref.Namespace, ref.Name)
	}

	// Auto-detect gzip compression (magic bytes 0x1f 0x8b)
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("creating gzip reader for key %q in Secret %s/%s: %w", ref.Key, ref.Namespace, ref.Name, err)
		}
		defer reader.Close()
		const maxDecompressedSize = 10 * 1024 * 1024 // 10 MiB
		limited := io.LimitReader(reader, maxDecompressedSize+1)
		decompressed, err := io.ReadAll(limited)
		if err != nil {
			return nil, fmt.Errorf("decompressing key %q in Secret %s/%s: %w", ref.Key, ref.Namespace, ref.Name, err)
		}
		if len(decompressed) > maxDecompressedSize {
			return nil, fmt.Errorf("decompressed data for key %q in Secret %s/%s exceeds maximum size (%d bytes)", ref.Key, ref.Namespace, ref.Name, maxDecompressedSize)
		}
		data = decompressed
	}

	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(data, &obj.Object); err != nil {
		return nil, fmt.Errorf("unmarshaling object from key %q in Secret %s/%s: %w", ref.Key, ref.Namespace, ref.Name, err)
	}

	return obj, nil
}

// EffectiveCollisionProtection resolves the collision protection value using
// the inheritance hierarchy: object > phase > spec > default ("Prevent").
func EffectiveCollisionProtection(cp ...ocv1.CollisionProtection) ocv1.CollisionProtection {
	ecp := ocv1.CollisionProtectionPrevent
	for _, c := range cp {
		if c != "" {
			ecp = c
		}
	}
	return ecp
}

// buildProgressionProbes creates a set of boxcutter probes from the fields provided in the COS's spec.progressionProbes.
// Returns nil and an error if encountered while attempting to build the probes.
func buildProgressionProbes(progressionProbes []ocv1.ProgressionProbe) (probing.And, error) {
	userProbes := probing.And{}
	if len(progressionProbes) < 1 {
		return userProbes, nil
	}
	for _, progressionProbe := range progressionProbes {
		// Collect all user assertions into a single 'And'
		assertions := probing.And{}
		for _, probe := range progressionProbe.Assertions {
			switch probe.Type {
			// Switch based on the union discriminator
			case ocv1.ProbeTypeConditionEqual:
				conditionProbe := probing.ConditionProbe(probe.ConditionEqual)
				assertions = append(assertions, &conditionProbe)
			case ocv1.ProbeTypeFieldsEqual:
				fieldsEqualProbe := probing.FieldsEqualProbe(probe.FieldsEqual)
				assertions = append(assertions, &fieldsEqualProbe)
			case ocv1.ProbeTypeFieldValue:
				fieldValueProbe := probing.FieldValueProbe(probe.FieldValue)
				assertions = append(assertions, &fieldValueProbe)
			default:
				return nil, fmt.Errorf("unknown progressionProbe assertion probe type: %s", probe.Type)
			}
		}

		// Create the selector probe based on user-requested type and provide the assertions
		var selectorProbe probing.Prober
		switch progressionProbe.Selector.Type {
		// Switch based on the union discriminator
		case ocv1.SelectorTypeGroupKind:
			selectorProbe = &probing.GroupKindSelector{
				GroupKind: schema.GroupKind(progressionProbe.Selector.GroupKind),
				Prober:    assertions,
			}
		case ocv1.SelectorTypeLabel:
			selector, err := metav1.LabelSelectorAsSelector(&progressionProbe.Selector.Label)
			if err != nil {
				return nil, fmt.Errorf("invalid label selector in progressionProbe (%v): %w", progressionProbe.Selector.Label, err)
			}
			selectorProbe = &probing.LabelSelector{
				Selector: selector,
				Prober:   assertions,
			}
		default:
			return nil, fmt.Errorf("unknown progressionProbe selector type: %s", progressionProbe.Selector.Type)
		}
		userProbes = append(userProbes, &probing.ObservedGenerationProbe{
			Prober: selectorProbe,
		})
	}
	return userProbes, nil
}

func setRetryingConditions(cos *ocv1.ClusterObjectSet, message string, isDeadlineExceeded bool) {
	markAsProgressing(cos, ocv1.ClusterObjectSetReasonRetrying, message, isDeadlineExceeded)
	if meta.FindStatusCondition(cos.Status.Conditions, ocv1.ClusterObjectSetTypeAvailable) != nil {
		markAsAvailableUnknown(cos, ocv1.ClusterObjectSetReasonReconciling, message)
	}
}

// durationUntilDeadline returns how much time is left before the progress deadline
// is exceeded. A negative duration means the deadline has already passed. If there
// is no deadline (progressDeadlineMinutes is 0 or the revision has already succeeded),
// it returns -1 and false.
func (c *ClusterObjectSetReconciler) durationUntilDeadline(cos *ocv1.ClusterObjectSet) (time.Duration, bool) {
	return durationUntilDeadline(c.Clock, cos)
}

// durationUntilDeadline returns how much time is left before the progress deadline
// is exceeded. A negative duration means the deadline has already passed. If there
// is no deadline (progressDeadlineMinutes is 0 or the revision has already succeeded),
// it returns -1 and false.
func durationUntilDeadline(clk clock.Clock, cos *ocv1.ClusterObjectSet) (time.Duration, bool) {
	pd := cos.Spec.ProgressDeadlineMinutes
	if pd <= 0 {
		return -1, false
	}
	// Succeeded is a latch — once set, it's never cleared. A revision that
	// has already succeeded should not be blocked by the deadline, even if
	// it temporarily goes back to InTransition (e.g., recovery after drift).
	if meta.IsStatusConditionTrue(cos.Status.Conditions, ocv1.ClusterObjectSetTypeSucceeded) {
		return -1, false
	}
	timeout := time.Duration(pd) * time.Minute
	return timeout - clk.Since(cos.CreationTimestamp.Time), true
}

// markAsProgressing sets the Progressing condition to True with the given reason.
//
// For non-terminal reasons (RollingOut, Retrying), if isDeadlineExceeded is true,
// the condition is set to Progressing=False/ProgressDeadlineExceeded instead. This
// prevents a reconcile loop where RollingOut and ProgressDeadlineExceeded overwrite
// each other on every cycle.
//
// Terminal reasons (Succeeded) are always applied. Unregistered reasons panic.
func markAsProgressing(cos *ocv1.ClusterObjectSet, reason, message string, isDeadlineExceeded bool) {
	switch reason {
	case ocv1.ReasonSucceeded:
		// Terminal — always apply.
	case ocv1.ReasonRollingOut, ocv1.ClusterObjectSetReasonRetrying:
		if isDeadlineExceeded {
			markAsNotProgressing(cos, ocv1.ReasonProgressDeadlineExceeded,
				fmt.Sprintf("Revision has not rolled out for %d minute(s).", cos.Spec.ProgressDeadlineMinutes))
			return
		}
	default:
		panic(fmt.Sprintf("unregistered progressing reason: %q", reason))
	}
	meta.SetStatusCondition(&cos.Status.Conditions, metav1.Condition{
		Type:               ocv1.ClusterObjectSetTypeProgressing,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cos.Generation,
	})
}

func markAsNotProgressing(cos *ocv1.ClusterObjectSet, reason, message string) bool {
	return meta.SetStatusCondition(&cos.Status.Conditions, metav1.Condition{
		Type:               ocv1.ClusterObjectSetTypeProgressing,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cos.Generation,
	})
}

func markAsAvailable(cos *ocv1.ClusterObjectSet, reason, message string) bool {
	return meta.SetStatusCondition(&cos.Status.Conditions, metav1.Condition{
		Type:               ocv1.ClusterObjectSetTypeAvailable,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cos.Generation,
	})
}

func markAsUnavailable(cos *ocv1.ClusterObjectSet, reason, message string) {
	meta.SetStatusCondition(&cos.Status.Conditions, metav1.Condition{
		Type:               ocv1.ClusterObjectSetTypeAvailable,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cos.Generation,
	})
}

func markAsAvailableUnknown(cos *ocv1.ClusterObjectSet, reason, message string) bool {
	return meta.SetStatusCondition(&cos.Status.Conditions, metav1.Condition{
		Type:               ocv1.ClusterObjectSetTypeAvailable,
		Status:             metav1.ConditionUnknown,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cos.Generation,
	})
}

func markAsArchived(cos *ocv1.ClusterObjectSet) bool {
	const msg = "revision is archived"
	updated := markAsNotProgressing(cos, ocv1.ClusterObjectSetReasonArchived, msg)
	return markAsAvailableUnknown(cos, ocv1.ClusterObjectSetReasonArchived, msg) || updated
}
