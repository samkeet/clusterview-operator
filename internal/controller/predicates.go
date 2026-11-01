package controller

import (
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/samkeet/clusterview-operator/internal/clusterstate"
)

// nodeChanged passes only Ready transitions. The kubelet updates its Node every
// ~10s just to refresh heartbeats. Ready is the only node data in the CR status, so
// anything else would only cause reconciles that write nothing.
func nodeChanged(old, cur *corev1.Node) bool {
	return clusterstate.NodeStatus(old) != clusterstate.NodeStatus(cur)
}

// podChanged passes changes to fields the controller aggregates (phase, node binding),
// plus the restart sum, so a crash loop that stays Running is still noticed.
func podChanged(old, cur *corev1.Pod) bool {
	return old.Status.Phase != cur.Status.Phase ||
		old.Spec.NodeName != cur.Spec.NodeName ||
		clusterstate.PodRestarts(old) != clusterstate.PodRestarts(cur)
}

// nodePredicate and podPredicate let create and delete through. An update that
// can't be type-asserted also passes, since a spare reconcile is cheap and a dropped
// event is not. Generic events have no source here.
var (
	nodePredicate = predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			old, ok1 := e.ObjectOld.(*corev1.Node)
			cur, ok2 := e.ObjectNew.(*corev1.Node)
			return !ok1 || !ok2 || nodeChanged(old, cur)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	podPredicate = predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			old, ok1 := e.ObjectOld.(*corev1.Pod)
			cur, ok2 := e.ObjectNew.(*corev1.Pod)
			return !ok1 || !ok2 || podChanged(old, cur)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
)
