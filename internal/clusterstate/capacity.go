package clusterstate

import (
	"cmp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourcehelper "k8s.io/component-helpers/resource"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
)

const (
	// NodePoolLabel names a node's pool on DOKS. It's hardcoded on purpose (minimal
	// scope); a flag is a small change if another provider is ever needed.
	NodePoolLabel = "doks.digitalocean.com/node-pool"
	// UnpooledPool groups nodes without a pool label (or with an empty one).
	// Parentheses aren't valid in label values, so no real pool can collide with it.
	UnpooledPool = "(unpooled)"
)

// describeOptions are the options kubectl 1.37's `describe node` passes to the
// helper, for each pod row and for the node totals. Using the same ones keeps our
// numbers identical to describe's, including during an in-place resize.
var describeOptions = resourcehelper.PodResourcesOptions{
	SkipPodLevelResources: false,
	UseStatusResources:    true,
}

// PodCapacity is a pod's effective requests and limits, as `kubectl describe node`
// shows them. An unset limit is 0, as in describe.
type PodCapacity struct {
	Namespace       string             `json:"namespace"`
	Name            string             `json:"name"`
	QOS             corev1.PodQOSClass `json:"qos"`
	CPURequestMilli int64              `json:"cpuRequestMilli"`
	CPULimitMilli   int64              `json:"cpuLimitMilli"`
	MemRequestBytes int64              `json:"memRequestBytes"`
	MemLimitBytes   int64              `json:"memLimitBytes"`
}

// PodRequestsLimits computes a pod's effective requests and limits with
// k8s.io/component-helpers, the code kubectl describe runs. It covers init and
// sidecar (restartable init) containers, pod overhead, pod-level resources and
// resources reported in status during an in-place resize. QoS is read from
// status.qosClass, which the API server sets at admission.
func PodRequestsLimits(p *corev1.Pod) PodCapacity {
	reqs := resourcehelper.PodRequests(p, describeOptions)
	limits := resourcehelper.PodLimits(p, describeOptions)
	return PodCapacity{
		Namespace:       p.Namespace,
		Name:            p.Name,
		QOS:             p.Status.QOSClass,
		CPURequestMilli: reqs.Cpu().MilliValue(),
		CPULimitMilli:   limits.Cpu().MilliValue(),
		MemRequestBytes: reqs.Memory().Value(),
		MemLimitBytes:   limits.Memory().Value(),
	}
}

// Terminated reports whether a pod has finished. kubectl describe node lists
// only non-terminated pods (phase is neither Succeeded nor Failed), and so do we.
func Terminated(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// Allocatable is what the scheduler can hand out on a node.
type Allocatable struct {
	CPUMilli    int64 `json:"cpuMilli"`
	MemoryBytes int64 `json:"memoryBytes"`
}

// CapacityTotals sums the effective requests and limits of a node's pods, as in
// describe's "Allocated resources" block.
type CapacityTotals struct {
	CPURequestMilli int64 `json:"cpuRequestMilli"`
	CPULimitMilli   int64 `json:"cpuLimitMilli"`
	MemRequestBytes int64 `json:"memRequestBytes"`
	MemLimitBytes   int64 `json:"memLimitBytes"`
}

// NodeCapacity is one row of the capacity map. Pods are sorted by namespace,
// then name, as describe lists them.
type NodeCapacity struct {
	Name        string                        `json:"name"`
	Status      observev1alpha1.NodeReadiness `json:"status"`
	Schedulable bool                          `json:"schedulable"`
	Allocatable Allocatable                   `json:"allocatable"`
	Totals      CapacityTotals                `json:"totals"`
	Pods        []PodCapacity                 `json:"pods"`
}

// PoolCapacity is a node pool. InstanceType is set only when every node in the
// pool has the same one.
type PoolCapacity struct {
	Name         string         `json:"name"`
	InstanceType string         `json:"instanceType,omitempty"`
	NodeCount    int32          `json:"nodeCount"`
	Nodes        []NodeCapacity `json:"nodes"`
}

// PoolsSnapshot is the /api/v1/pools body. Filtered is true when the ClusterView
// limits namespaces, so node totals cover only the included namespaces.
type PoolsSnapshot struct {
	Filtered bool           `json:"filtered"`
	Pools    []PoolCapacity `json:"pools"`
}

// PoolName returns a node's pool, or UnpooledPool if it has none.
func PoolName(n *corev1.Node) string {
	if name := n.Labels[NodePoolLabel]; name != "" {
		return name
	}
	return UnpooledPool
}

// BuildPools groups nodes into pools, each node with its non-terminated pods that
// pass the namespace filter. Unscheduled pods and pods bound to a node that isn't
// in the list are left out, since they use no listed node's capacity. Pools are
// sorted by name with UnpooledPool last, and nodes by name, so the output (and
// the API's ETag) is deterministic.
func BuildPools(nodes []corev1.Node, pods []corev1.Pod, namespaces []string) PoolsSnapshot {
	perNode := make(map[string][]PodCapacity, len(nodes))
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName == "" || Terminated(p) || !InNamespaces(p.Namespace, namespaces) {
			continue
		}
		perNode[p.Spec.NodeName] = append(perNode[p.Spec.NodeName], PodRequestsLimits(p))
	}

	byName := map[string]*PoolCapacity{}
	mixedType := map[string]bool{}
	for i := range nodes {
		n := &nodes[i]
		name := PoolName(n)
		instanceType := n.Labels[corev1.LabelInstanceTypeStable]
		pool := byName[name]
		if pool == nil {
			pool = &PoolCapacity{Name: name, InstanceType: instanceType}
			byName[name] = pool
		} else if pool.InstanceType != instanceType {
			mixedType[name] = true
		}
		pool.NodeCount++
		pool.Nodes = append(pool.Nodes, newNodeCapacity(n, perNode[n.Name]))
	}

	pools := make([]PoolCapacity, 0, len(byName))
	for name, pool := range byName {
		if mixedType[name] {
			pool.InstanceType = ""
		}
		slices.SortFunc(pool.Nodes, func(a, b NodeCapacity) int { return strings.Compare(a.Name, b.Name) })
		pools = append(pools, *pool)
	}
	slices.SortFunc(pools, func(a, b PoolCapacity) int {
		return cmp.Or(
			cmp.Compare(unpooledRank(a.Name), unpooledRank(b.Name)),
			strings.Compare(a.Name, b.Name),
		)
	})
	return PoolsSnapshot{Filtered: len(namespaces) > 0, Pools: pools}
}

// unpooledRank sorts the unpooled group after every real pool.
func unpooledRank(pool string) int {
	if pool == UnpooledPool {
		return 1
	}
	return 0
}

func newNodeCapacity(n *corev1.Node, pods []PodCapacity) NodeCapacity {
	// describe falls back to capacity while a node hasn't reported allocatable.
	alloc := n.Status.Allocatable
	if len(alloc) == 0 {
		alloc = n.Status.Capacity
	}
	if pods == nil {
		pods = []PodCapacity{} // [] rather than null in the JSON
	}
	slices.SortFunc(pods, func(a, b PodCapacity) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	var t CapacityTotals
	for _, p := range pods {
		t.CPURequestMilli += p.CPURequestMilli
		t.CPULimitMilli += p.CPULimitMilli
		t.MemRequestBytes += p.MemRequestBytes
		t.MemLimitBytes += p.MemLimitBytes
	}
	return NodeCapacity{
		Name:        n.Name,
		Status:      NodeStatus(n),
		Schedulable: !n.Spec.Unschedulable,
		Allocatable: Allocatable{CPUMilli: alloc.Cpu().MilliValue(), MemoryBytes: alloc.Memory().Value()},
		Totals:      t,
		Pods:        pods,
	}
}
