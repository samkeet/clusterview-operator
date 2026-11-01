// Package clusterstate turns Nodes and Pods into the aggregated view shared by the
// controller (CR status) and the HTTP API. It is pure: no client, no clock, so both
// consumers compute identical numbers and it can be table-tested in isolation.
package clusterstate

import (
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
)

// Resources is a flattened resource list. Plain integers keep the JSON trivial for
// the frontend, which would otherwise have to parse Quantity strings like "7948Mi".
type Resources struct {
	CPUMilli    int64 `json:"cpuMilli"`
	MemoryBytes int64 `json:"memoryBytes"`
	Pods        int64 `json:"pods"`
}

// Condition is a trimmed corev1.NodeCondition. Heartbeat time is dropped on purpose:
// it changes every ~10s and would defeat change detection and HTTP ETags.
type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// NodeInfo is the per-node view.
type NodeInfo struct {
	Name         string                        `json:"name"`
	Status       observev1alpha1.NodeReadiness `json:"status"`
	Schedulable  bool                          `json:"schedulable"`
	InstanceType string                        `json:"instanceType,omitempty"`
	Arch         string                        `json:"arch,omitempty"`
	KubeletVer   string                        `json:"kubeletVersion,omitempty"`
	Conditions   []Condition                   `json:"conditions"`
	Capacity     Resources                     `json:"capacity"`
	Allocatable  Resources                     `json:"allocatable"`
	PodCount     int32                         `json:"podCount"`
	CreatedAt    time.Time                     `json:"createdAt"`
}

// PodInfo is the per-pod view shown when a node is selected.
type PodInfo struct {
	Namespace string          `json:"namespace"`
	Name      string          `json:"name"`
	NodeName  string          `json:"nodeName,omitempty"`
	Phase     corev1.PodPhase `json:"phase"`
	Restarts  int32           `json:"restarts"`
	CreatedAt time.Time       `json:"createdAt"`
}

// Summary holds cluster-wide counts.
type Summary struct {
	NodeCount       int32            `json:"nodeCount"`
	ReadyNodes      int32            `json:"readyNodes"`
	NotReadyNodes   int32            `json:"notReadyNodes"`
	PodCount        int32            `json:"podCount"`
	UnscheduledPods int32            `json:"unscheduledPods"`
	PodPhases       map[string]int32 `json:"podPhases"`
}

// Snapshot is the full aggregated view. Nodes are sorted by name so output is
// deterministic (stable diffs for the controller, stable ETags for the API).
type Snapshot struct {
	Summary Summary    `json:"summary"`
	Nodes   []NodeInfo `json:"nodes"`
}

// NodeStatus maps the Ready condition to three states. A missing condition is
// Unknown rather than NotReady because it usually means the node just registered.
func NodeStatus(n *corev1.Node) observev1alpha1.NodeReadiness {
	for _, c := range n.Status.Conditions {
		if c.Type != corev1.NodeReady {
			continue
		}
		switch c.Status {
		case corev1.ConditionTrue:
			return observev1alpha1.NodeReady
		case corev1.ConditionFalse:
			return observev1alpha1.NodeNotReady
		default:
			return observev1alpha1.NodeUnknown
		}
	}
	return observev1alpha1.NodeUnknown
}

// PodRestarts sums restarts across init and app containers. Init containers count
// because a crash-looping init container is exactly what an operator wants surfaced.
func PodRestarts(p *corev1.Pod) int32 {
	var n int32
	for _, cs := range p.Status.InitContainerStatuses {
		n += cs.RestartCount
	}
	for _, cs := range p.Status.ContainerStatuses {
		n += cs.RestartCount
	}
	return n
}

// InNamespaces reports whether ns passes the filter. An empty filter means all namespaces.
func InNamespaces(ns string, namespaces []string) bool {
	return len(namespaces) == 0 || slices.Contains(namespaces, ns)
}

// NewPodInfo converts a Pod.
func NewPodInfo(p *corev1.Pod) PodInfo {
	return PodInfo{
		Namespace: p.Namespace,
		Name:      p.Name,
		NodeName:  p.Spec.NodeName,
		Phase:     p.Status.Phase,
		Restarts:  PodRestarts(p),
		CreatedAt: p.CreationTimestamp.UTC(),
	}
}

// NewNodeInfo converts a Node. podCount is passed in because counting needs the pod list.
func NewNodeInfo(n *corev1.Node, podCount int32) NodeInfo {
	conds := make([]Condition, 0, len(n.Status.Conditions))
	for _, c := range n.Status.Conditions {
		conds = append(conds, Condition{
			Type:               string(c.Type),
			Status:             string(c.Status),
			Reason:             c.Reason,
			Message:            c.Message,
			LastTransitionTime: c.LastTransitionTime.UTC(),
		})
	}
	return NodeInfo{
		Name:         n.Name,
		Status:       NodeStatus(n),
		Schedulable:  !n.Spec.Unschedulable,
		InstanceType: n.Labels[corev1.LabelInstanceTypeStable],
		Arch:         n.Status.NodeInfo.Architecture,
		KubeletVer:   n.Status.NodeInfo.KubeletVersion,
		Conditions:   conds,
		Capacity:     toResources(n.Status.Capacity),
		Allocatable:  toResources(n.Status.Allocatable),
		PodCount:     podCount,
		CreatedAt:    n.CreationTimestamp.UTC(),
	}
}

// Build aggregates nodes and the pods that pass the namespace filter. Pods bound to a
// node that is not in the list (e.g. node just deleted) still count toward totals.
func Build(nodes []corev1.Node, pods []corev1.Pod, namespaces []string) Snapshot {
	perNode := make(map[string]int32, len(nodes))
	sum := Summary{PodPhases: map[string]int32{}}
	for i := range pods {
		p := &pods[i]
		if !InNamespaces(p.Namespace, namespaces) {
			continue
		}
		sum.PodCount++
		sum.PodPhases[string(p.Status.Phase)]++
		if p.Spec.NodeName == "" {
			sum.UnscheduledPods++
			continue
		}
		perNode[p.Spec.NodeName]++
	}

	infos := make([]NodeInfo, 0, len(nodes))
	for i := range nodes {
		info := NewNodeInfo(&nodes[i], perNode[nodes[i].Name])
		sum.NodeCount++
		if info.Status == observev1alpha1.NodeReady {
			sum.ReadyNodes++
		} else {
			sum.NotReadyNodes++
		}
		infos = append(infos, info)
	}
	slices.SortFunc(infos, func(a, b NodeInfo) int { return strings.Compare(a.Name, b.Name) })
	return Snapshot{Summary: sum, Nodes: infos}
}

func toResources(rl corev1.ResourceList) Resources {
	return Resources{
		CPUMilli:    rl.Cpu().MilliValue(),
		MemoryBytes: rl.Memory().Value(),
		Pods:        rl.Pods().Value(),
	}
}
