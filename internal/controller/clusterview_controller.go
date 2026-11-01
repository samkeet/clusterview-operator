/*
Copyright 2026.

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

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
	"github.com/samkeet/clusterview-operator/internal/clusterstate"
)

// ClusterViewReconciler writes a throttled summary of Nodes and Pods into each
// ClusterView's status. All reads come from the manager's informer cache, so the
// status writes are the only API server load it adds.
type ClusterViewReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Clock is injectable so tests can step through the throttle window. Nil means
	// the real clock, so cmd/main.go doesn't have to set it.
	Clock clock.PassiveClock

	writes writeTracker
}

// +kubebuilder:rbac:groups=observe.samkeet.org,resources=clusterviews,verbs=get;list;watch
// Status is only ever written with Status().Patch and never read through the
// subresource, so patch is the one verb it needs.
// +kubebuilder:rbac:groups=observe.samkeet.org,resources=clusterviews/status,verbs=patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=list;watch

// Reconcile is level-triggered: each run rebuilds the summary from the whole cache,
// so a deferred or coalesced run never loses an event.
func (r *ClusterViewReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cv observev1alpha1.ClusterView
	if err := r.Get(ctx, req.NamespacedName, &cv); err != nil {
		if apierrors.IsNotFound(err) {
			r.writes.forget(req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting ClusterView %q: %w", req.Name, err)
	}

	now := r.now()
	desired := cv.Status.DeepCopy()
	observeErr := r.observe(ctx, &cv, desired, now)

	last := lastWriteTime(cv.Status.LastUpdated, r.writes.get(cv.Name, cv.UID))
	write, after := decide(now, last, cv.Spec.RefreshInterval.Duration,
		!contentEqual(&cv.Status, desired), cv.Generation != cv.Status.ObservedGeneration)
	if !write {
		if observeErr != nil {
			return ctrl.Result{}, observeErr
		}
		log.V(1).Info("Status write skipped", "requeueAfter", after)
		return ctrl.Result{RequeueAfter: after}, nil
	}

	desired.LastUpdated = &metav1.Time{Time: now}
	updated := cv.DeepCopy()
	updated.Status = *desired
	// The optimistic lock turns a stale cached base into a 409. A plain merge patch
	// would diff against the stale copy and could leave out a field that differs on
	// the server.
	patch := client.MergeFromWithOptions(&cv, client.MergeFromWithOptimisticLock{})
	if err := r.Status().Patch(ctx, updated, patch); err != nil {
		if apierrors.IsConflict(err) && observeErr == nil {
			log.V(1).Info("Status patch conflicted with a newer version, retrying")
			return ctrl.Result{RequeueAfter: conflictRetry}, nil
		}
		return ctrl.Result{}, fmt.Errorf("patching ClusterView %q status: %w", cv.Name, err)
	}
	r.writes.record(cv.Name, cv.UID, now)
	log.V(1).Info("Status written", "nodes", desired.NodeCount, "pods", desired.PodCount)

	if observeErr != nil {
		return ctrl.Result{}, observeErr
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// observe fills st from the cache. On a list failure it keeps the previous counts
// and sets Ready=False, so the CR shows the failure instead of looking healthy with
// stale numbers.
func (r *ClusterViewReconciler) observe(ctx context.Context, cv *observev1alpha1.ClusterView,
	st *observev1alpha1.ClusterViewStatus, now time.Time) error {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		setReady(st, cv.Generation, metav1.ConditionFalse, reasonListNodesFailed, "Listing nodes from the cache failed", now)
		return fmt.Errorf("listing nodes: %w", err)
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods); err != nil {
		setReady(st, cv.Generation, metav1.ConditionFalse, reasonListPodsFailed, "Listing pods from the cache failed", now)
		return fmt.Errorf("listing pods: %w", err)
	}

	applySnapshot(st, clusterstate.Build(nodes.Items, pods.Items, cv.Spec.Namespaces))
	st.ObservedGeneration = cv.Generation
	setReady(st, cv.Generation, metav1.ConditionTrue, reasonReconciled, "Status reflects the informer cache", now)
	return nil
}

func (r *ClusterViewReconciler) now() time.Time {
	if r.Clock == nil {
		return clock.RealClock{}.Now()
	}
	return r.Clock.Now()
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterViewReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Generation-only, so our own status patches don't retrigger a reconcile.
		For(&observev1alpha1.ClusterView{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.mapAll), builder.WithPredicates(nodePredicate)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.mapAll), builder.WithPredicates(podPredicate)).
		Named("clusterview").
		Complete(r)
}

// mapAll fans a Node or Pod event out to every ClusterView. Each view summarises
// the whole cluster, so any of them may be affected. The per-CR namespace filter is
// applied in Reconcile, where a filtered-out pod simply produces no write.
func (r *ClusterViewReconciler) mapAll(ctx context.Context, _ client.Object) []reconcile.Request {
	var views observev1alpha1.ClusterViewList
	if err := r.List(ctx, &views); err != nil {
		// A map func can't return an error. The 5m backstop recovers the missed event.
		logf.FromContext(ctx).Error(err, "Failed to list ClusterViews for a node or pod event")
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(views.Items))
	for i := range views.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&views.Items[i])})
	}
	return reqs
}
