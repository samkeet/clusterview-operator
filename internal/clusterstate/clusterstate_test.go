package clusterstate

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
)

const (
	nsDefault = "default"
	nsSystem  = "kube-system"
	nodeEmpty = "empty"
)

func node(name string, ready *corev1.ConditionStatus) corev1.Node {
	n := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if ready != nil {
		n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: *ready}}
	}
	return n
}

func pod(ns, name, nodeName string, phase corev1.PodPhase, restarts ...int32) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.PodSpec{NodeName: nodeName},
		Status:     corev1.PodStatus{Phase: phase},
	}
	for _, r := range restarts {
		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{RestartCount: r})
	}
	return p
}

func status(s corev1.ConditionStatus) *corev1.ConditionStatus { return new(s) }

func TestNodeStatus(t *testing.T) {
	tests := []struct {
		name string
		node corev1.Node
		want observev1alpha1.NodeReadiness
	}{
		{"ready true", node("n", status(corev1.ConditionTrue)), observev1alpha1.NodeReady},
		{"ready false", node("n", status(corev1.ConditionFalse)), observev1alpha1.NodeNotReady},
		{"ready unknown", node("n", status(corev1.ConditionUnknown)), observev1alpha1.NodeUnknown},
		{"no conditions", node("n", nil), observev1alpha1.NodeUnknown},
		{
			name: "only pressure conditions",
			node: corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
			}}},
			want: observev1alpha1.NodeUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NodeStatus(&tt.node); got != tt.want {
				t.Errorf("NodeStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPodRestarts(t *testing.T) {
	withInit := pod("ns", "p", "n", corev1.PodRunning, 2, 3)
	withInit.Status.InitContainerStatuses = []corev1.ContainerStatus{{RestartCount: 4}}

	tests := []struct {
		name string
		pod  corev1.Pod
		want int32
	}{
		{"no containers", pod("ns", "p", "n", corev1.PodPending), 0},
		{"single container", pod("ns", "p", "n", corev1.PodRunning, 5), 5},
		{"init and app containers", withInit, 9},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PodRestarts(&tt.pod); got != tt.want {
				t.Errorf("PodRestarts() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestInNamespaces(t *testing.T) {
	tests := []struct {
		name   string
		ns     string
		filter []string
		want   bool
	}{
		{"empty filter matches all", nsSystem, nil, true},
		{"listed", nsDefault, []string{nsDefault, "apps"}, true},
		{"not listed", nsSystem, []string{nsDefault}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := InNamespaces(tt.ns, tt.filter); got != tt.want {
				t.Errorf("InNamespaces(%q, %v) = %v, want %v", tt.ns, tt.filter, got, tt.want)
			}
		})
	}
}

func TestBuild(t *testing.T) {
	nodes := []corev1.Node{
		node("b", status(corev1.ConditionFalse)),
		node("a", status(corev1.ConditionTrue)),
		node("c", status(corev1.ConditionUnknown)),
		node(nodeEmpty, status(corev1.ConditionTrue)),
	}
	pods := []corev1.Pod{
		pod(nsDefault, "p1", "a", corev1.PodRunning),
		pod(nsDefault, "p2", "a", corev1.PodRunning),
		pod(nsSystem, "p3", "b", corev1.PodFailed),
		pod(nsDefault, "p4", "", corev1.PodPending),
		pod(nsDefault, "p5", "gone", corev1.PodSucceeded),
	}

	tests := []struct {
		name       string
		namespaces []string
		want       Summary
		wantPods   map[string]int32
	}{
		{
			name: "all namespaces",
			want: Summary{
				NodeCount: 4, ReadyNodes: 2, NotReadyNodes: 2, PodCount: 5, UnscheduledPods: 1,
				PodPhases: map[string]int32{"Running": 2, "Failed": 1, "Pending": 1, "Succeeded": 1},
			},
			wantPods: map[string]int32{"a": 2, "b": 1, "c": 0, nodeEmpty: 0},
		},
		{
			name:       "namespace filter applies to pods only",
			namespaces: []string{nsSystem},
			want: Summary{
				NodeCount: 4, ReadyNodes: 2, NotReadyNodes: 2, PodCount: 1,
				PodPhases: map[string]int32{"Failed": 1},
			},
			wantPods: map[string]int32{"a": 0, "b": 1, "c": 0, nodeEmpty: 0},
		},
		{
			name:       "filter matching nothing",
			namespaces: []string{"nope"},
			want:       Summary{NodeCount: 4, ReadyNodes: 2, NotReadyNodes: 2, PodPhases: map[string]int32{}},
			wantPods:   map[string]int32{"a": 0, "b": 0, "c": 0, nodeEmpty: 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Build(nodes, pods, tt.namespaces)
			s := got.Summary
			if s.NodeCount != tt.want.NodeCount || s.ReadyNodes != tt.want.ReadyNodes ||
				s.NotReadyNodes != tt.want.NotReadyNodes || s.PodCount != tt.want.PodCount ||
				s.UnscheduledPods != tt.want.UnscheduledPods {
				t.Errorf("summary = %+v, want %+v", s, tt.want)
			}
			if len(s.PodPhases) != len(tt.want.PodPhases) {
				t.Errorf("podPhases = %v, want %v", s.PodPhases, tt.want.PodPhases)
			}
			for k, v := range tt.want.PodPhases {
				if s.PodPhases[k] != v {
					t.Errorf("podPhases[%s] = %d, want %d", k, s.PodPhases[k], v)
				}
			}
			for i := 1; i < len(got.Nodes); i++ {
				if got.Nodes[i-1].Name >= got.Nodes[i].Name {
					t.Fatalf("nodes not sorted: %q before %q", got.Nodes[i-1].Name, got.Nodes[i].Name)
				}
			}
			for _, n := range got.Nodes {
				if n.PodCount != tt.wantPods[n.Name] {
					t.Errorf("node %s podCount = %d, want %d", n.Name, n.PodCount, tt.wantPods[n.Name])
				}
			}
		})
	}
}

func TestNewNodeInfo(t *testing.T) {
	n := node("n1", status(corev1.ConditionTrue))
	n.Labels = map[string]string{corev1.LabelInstanceTypeStable: "s-4vcpu-8gb"}
	n.Spec.Unschedulable = true
	n.Status.NodeInfo.Architecture = "amd64"
	n.Status.Capacity = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
	}
	n.Status.Allocatable = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("3890m"),
		corev1.ResourceMemory: resource.MustParse("6Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
	}

	got := NewNodeInfo(&n, 7)
	if got.Schedulable {
		t.Error("cordoned node reported schedulable")
	}
	if got.Status != observev1alpha1.NodeReady {
		t.Errorf("status = %q, want Ready (cordon must not affect readiness)", got.Status)
	}
	if got.Capacity.CPUMilli != 4000 || got.Allocatable.CPUMilli != 3890 {
		t.Errorf("cpu capacity/allocatable = %d/%d, want 4000/3890", got.Capacity.CPUMilli, got.Allocatable.CPUMilli)
	}
	if got.Capacity.MemoryBytes != 8<<30 || got.Allocatable.Pods != 110 {
		t.Errorf("memory/pods = %d/%d", got.Capacity.MemoryBytes, got.Allocatable.Pods)
	}
	if got.InstanceType != "s-4vcpu-8gb" || got.Arch != "amd64" || got.PodCount != 7 {
		t.Errorf("unexpected node info: %+v", got)
	}
}
