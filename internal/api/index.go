package api

import (
	corev1 "k8s.io/api/core/v1"
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
