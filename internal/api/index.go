package api

import (
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodNodeNameField indexes cached Pods by the node they're bound to, so a per-node
// pod lookup scales with that node's pods instead of every pod in the cluster.
const PodNodeNameField = "spec.nodeName"

// PodNodeName is the indexer for PodNodeNameField. Unscheduled pods aren't indexed,
// because nothing ever looks up pods by an empty node name.
func PodNodeName(obj client.Object) []string {
	p, ok := obj.(*corev1.Pod)
	if !ok || p.Spec.NodeName == "" {
		return nil
	}
	return []string{p.Spec.NodeName}
}

// EventRegardingField indexes cached Events by the object they're about, so a
// pod's events are found without scanning every event in the cluster.
const EventRegardingField = "regarding.key"

// RegardingKey is the EventRegardingField value for an object. Kind is part of
// the key so node events can share the index later (namespace empty).
func RegardingKey(kind, namespace, name string) string {
	return kind + "/" + namespace + "/" + name
}

// EventRegarding is the indexer for EventRegardingField. Events with no kind or
// name in regarding can't be looked up, so they aren't indexed.
func EventRegarding(obj client.Object) []string {
	e, ok := obj.(*eventsv1.Event)
	if !ok || e.Regarding.Kind == "" || e.Regarding.Name == "" {
		return nil
	}
	return []string{RegardingKey(e.Regarding.Kind, e.Regarding.Namespace, e.Regarding.Name)}
}
