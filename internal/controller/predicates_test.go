package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

func readyNode(status corev1.ConditionStatus) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1", ResourceVersion: "1"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeReady, Status: status,
				LastHeartbeatTime: metav1.NewTime(testNow),
			}},
			Capacity: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
		},
	}
}

func TestNodePredicateUpdate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1.Node)
		want   bool
	}{
		{"heartbeat only", func(n *corev1.Node) {
			n.ResourceVersion = "2"
			n.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(testNow.Add(10 * time.Second))
		}, false},
		{"capacity only", func(n *corev1.Node) {
			n.Status.Capacity[corev1.ResourceCPU] = resource.MustParse("4")
			n.Status.Allocatable = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3")}
		}, false},
		{"cordon only", func(n *corev1.Node) { n.Spec.Unschedulable = true }, false},
		{"label only", func(n *corev1.Node) { n.Labels = map[string]string{"a": "b"} }, false},
		{"ready to not ready", func(n *corev1.Node) { n.Status.Conditions[0].Status = corev1.ConditionFalse }, true},
		{"ready to unknown", func(n *corev1.Node) { n.Status.Conditions[0].Status = corev1.ConditionUnknown }, true},
		{"ready condition removed", func(n *corev1.Node) { n.Status.Conditions = nil }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := readyNode(corev1.ConditionTrue)
			cur := old.DeepCopy()
			tt.mutate(cur)
			if got := nodePredicate.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: cur}); got != tt.want {
				t.Errorf("Update() = %v, want %v", got, tt.want)
			}
		})
	}
}

func basePod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "ns", ResourceVersion: "1"},
		Spec:       corev1.PodSpec{NodeName: "n1"},
		Status: corev1.PodStatus{
			Phase:                 corev1.PodRunning,
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "init", RestartCount: 1}},
			ContainerStatuses:     []corev1.ContainerStatus{{Name: "app", RestartCount: 2}},
		},
	}
}

func TestPodPredicateUpdate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*corev1.Pod)
		want   bool
	}{
		{"label only", func(p *corev1.Pod) { p.Labels = map[string]string{"a": "b"}; p.ResourceVersion = "2" }, false},
		{"condition only", func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}, false},
		{"restarts move between containers, sum unchanged", func(p *corev1.Pod) {
			p.Status.InitContainerStatuses[0].RestartCount = 0
			p.Status.ContainerStatuses[0].RestartCount = 3
		}, false},
		{"phase", func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed }, true},
		{"node binding", func(p *corev1.Pod) { p.Spec.NodeName = "n2" }, true},
		{"app container restart", func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 3 }, true},
		{"init container restart", func(p *corev1.Pod) { p.Status.InitContainerStatuses[0].RestartCount = 2 }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := basePod()
			cur := old.DeepCopy()
			tt.mutate(cur)
			if got := podPredicate.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: cur}); got != tt.want {
				t.Errorf("Update() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPredicatesCreateDeleteGeneric(t *testing.T) {
	tests := []struct {
		name string
		p    predicate.Funcs
		obj  client.Object
	}{
		{"node", nodePredicate, readyNode(corev1.ConditionTrue)},
		{"pod", podPredicate, basePod()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.p.Create(event.CreateEvent{Object: tt.obj}) {
				t.Error("create should pass")
			}
			if !tt.p.Delete(event.DeleteEvent{Object: tt.obj}) {
				t.Error("delete should pass")
			}
			if tt.p.Generic(event.GenericEvent{Object: tt.obj}) {
				t.Error("generic should not pass")
			}
		})
	}
}
