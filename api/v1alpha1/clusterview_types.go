/*
Copyright 2026 Samkeet Shah. All rights reserved. See LICENSE.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ClusterViewSpec configures what the operator observes.
// +kubebuilder:validation:XValidation:rule="!has(self.refreshInterval) || (duration(self.refreshInterval) >= duration('5s') && duration(self.refreshInterval) <= duration('10m'))",message="refreshInterval must be between 5s and 10m"
type ClusterViewSpec struct {
	// namespaces limits which pods are counted. Empty means all namespaces.
	// Nodes are always cluster-wide, since a namespace filter on nodes has no meaning.
	// +optional
	// +listType=set
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MaxLength=63
	Namespaces []string `json:"namespaces,omitempty"`

	// refreshInterval is the minimum time between status writes, not a polling
	// interval. Reads come from the informer cache. This only caps how often the
	// summary is written back to the API server, so bursts of pod churn coalesce.
	// +optional
	// +kubebuilder:default="10s"
	RefreshInterval metav1.Duration `json:"refreshInterval,omitzero"`
}

// NodeReadiness is derived from the node's Ready condition.
// +kubebuilder:validation:Enum=Ready;NotReady;Unknown
type NodeReadiness string

const (
	// NodeReady means the Ready condition is True.
	NodeReady NodeReadiness = "Ready"
	// NodeNotReady means the Ready condition is False: the kubelet reports itself unhealthy.
	NodeNotReady NodeReadiness = "NotReady"
	// NodeUnknown means the Ready condition is Unknown or missing: the control plane
	// lost contact with the kubelet, or the node just registered.
	NodeUnknown NodeReadiness = "Unknown"
)

// NodeSummary is the per-node slice of the status. It holds counts only. Pod and
// resource detail is served by the HTTP API so the CR stays small on big clusters.
type NodeSummary struct {
	// name of the node.
	Name string `json:"name"`
	// status derived from the node's Ready condition.
	Status NodeReadiness `json:"status"`
	// podCount is the number of pods bound to this node that pass the namespace filter.
	PodCount int32 `json:"podCount"`
}

// ClusterViewStatus is an aggregated, throttled sample of cluster state.
type ClusterViewStatus struct {
	// observedGeneration is the spec generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// lastUpdated is when the status content last changed (and so was last written).
	// The controller also uses it as the start of the write-throttle window.
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`

	// +optional
	NodeCount int32 `json:"nodeCount"`
	// +optional
	ReadyNodes int32 `json:"readyNodes"`
	// notReadyNodes counts nodes whose status is NotReady or Unknown.
	// +optional
	NotReadyNodes int32 `json:"notReadyNodes"`
	// podCount is the number of pods that pass the namespace filter, including unscheduled ones.
	// +optional
	PodCount int32 `json:"podCount"`

	// podPhases maps a pod phase (Pending, Running, Succeeded, Failed, Unknown) to a count.
	// +optional
	PodPhases map[string]int32 `json:"podPhases,omitempty"`

	// nodes holds per-node readiness and pod counts.
	// +optional
	// +listType=map
	// +listMapKey=name
	Nodes []NodeSummary `json:"nodes,omitempty"`

	// conditions: Ready=True when the last reconcile observed and recorded state successfully.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=cv
// +kubebuilder:printcolumn:name="Nodes",type=integer,JSONPath=`.status.nodeCount`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyNodes`
// +kubebuilder:printcolumn:name="Pods",type=integer,JSONPath=`.status.podCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterView is the Schema for the clusterviews API
type ClusterView struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines what to observe
	// +optional
	Spec ClusterViewSpec `json:"spec"`

	// status is the observed, aggregated cluster state
	// +optional
	Status ClusterViewStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ClusterViewList contains a list of ClusterView
type ClusterViewList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ClusterView `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ClusterView{}, &ClusterViewList{})
		return nil
	})
}
