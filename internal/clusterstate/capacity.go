package clusterstate

import (
	corev1 "k8s.io/api/core/v1"
	resourcehelper "k8s.io/component-helpers/resource"
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
