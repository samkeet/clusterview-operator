package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
	"github.com/samkeet/clusterview-operator/internal/clusterstate"
)

var testNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func baseStatus() *observev1alpha1.ClusterViewStatus {
	return &observev1alpha1.ClusterViewStatus{
		ObservedGeneration: 1,
		LastUpdated:        &metav1.Time{Time: testNow},
		NodeCount:          2,
		ReadyNodes:         1,
		NotReadyNodes:      1,
		PodCount:           3,
		PodPhases:          map[string]int32{string(corev1.PodRunning): 2, string(corev1.PodPending): 1},
		Nodes: []observev1alpha1.NodeSummary{
			{Name: "a", Status: observev1alpha1.NodeReady, PodCount: 2},
			{Name: "b", Status: observev1alpha1.NodeNotReady, PodCount: 1},
		},
		Conditions: []metav1.Condition{{
			Type: conditionReady, Status: metav1.ConditionTrue, Reason: reasonReconciled,
			ObservedGeneration: 1, LastTransitionTime: metav1.NewTime(testNow),
		}},
	}
}

func TestContentEqual(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*observev1alpha1.ClusterViewStatus)
		want   bool
	}{
		{"identical", func(*observev1alpha1.ClusterViewStatus) {}, true},
		{"only lastUpdated differs", func(s *observev1alpha1.ClusterViewStatus) {
			s.LastUpdated = &metav1.Time{Time: testNow.Add(time.Hour)}
		}, true},
		{"lastUpdated nil", func(s *observev1alpha1.ClusterViewStatus) { s.LastUpdated = nil }, true},
		{"only condition time differs", func(s *observev1alpha1.ClusterViewStatus) {
			s.Conditions[0].LastTransitionTime = metav1.NewTime(testNow.Add(time.Hour))
		}, true},
		{"node count", func(s *observev1alpha1.ClusterViewStatus) { s.NodeCount = 3 }, false},
		{"ready nodes", func(s *observev1alpha1.ClusterViewStatus) { s.ReadyNodes = 2 }, false},
		{"pod count", func(s *observev1alpha1.ClusterViewStatus) { s.PodCount = 4 }, false},
		{"phase count", func(s *observev1alpha1.ClusterViewStatus) { s.PodPhases[string(corev1.PodRunning)] = 3 }, false},
		{"phase removed", func(s *observev1alpha1.ClusterViewStatus) { delete(s.PodPhases, string(corev1.PodPending)) }, false},
		{"node status", func(s *observev1alpha1.ClusterViewStatus) {
			s.Nodes[1].Status = observev1alpha1.NodeUnknown
		}, false},
		{"node pod count", func(s *observev1alpha1.ClusterViewStatus) { s.Nodes[0].PodCount = 5 }, false},
		{"observed generation", func(s *observev1alpha1.ClusterViewStatus) { s.ObservedGeneration = 2 }, false},
		{"condition reason", func(s *observev1alpha1.ClusterViewStatus) {
			s.Conditions[0].Reason = reasonListPodsFailed
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := baseStatus()
			tt.mutate(b)
			if got := contentEqual(baseStatus(), b); got != tt.want {
				t.Errorf("contentEqual() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The API server drops empty omitempty fields, while Build returns an empty map.
// Treating those as different would make an empty cluster write on every reconcile.
func TestContentEqualNilVersusEmpty(t *testing.T) {
	a := &observev1alpha1.ClusterViewStatus{}
	b := &observev1alpha1.ClusterViewStatus{PodPhases: map[string]int32{}, Nodes: []observev1alpha1.NodeSummary{}}
	if !contentEqual(a, b) {
		t.Error("nil and empty collections should compare equal")
	}
}

func TestApplySnapshot(t *testing.T) {
	snap := clusterstate.Snapshot{
		Summary: clusterstate.Summary{
			NodeCount: 2, ReadyNodes: 1, NotReadyNodes: 1, PodCount: 3, UnscheduledPods: 0,
			PodPhases: map[string]int32{string(corev1.PodRunning): 2, string(corev1.PodPending): 1},
		},
		Nodes: []clusterstate.NodeInfo{
			{Name: "a", Status: observev1alpha1.NodeReady, PodCount: 2, Schedulable: true},
			{Name: "b", Status: observev1alpha1.NodeNotReady, PodCount: 1},
		},
	}
	got := &observev1alpha1.ClusterViewStatus{}
	applySnapshot(got, snap)
	want := baseStatus()
	want.ObservedGeneration, want.LastUpdated, want.Conditions = 0, nil, nil
	if !equality.Semantic.DeepEqual(got, want) {
		t.Errorf("applySnapshot() = %+v, want %+v", got, want)
	}

	applySnapshot(got, clusterstate.Snapshot{Summary: clusterstate.Summary{PodPhases: map[string]int32{}}})
	if got.PodPhases != nil || got.Nodes != nil {
		t.Errorf("empty snapshot should clear collections, got %+v", got)
	}
}

func TestSetReady(t *testing.T) {
	later := testNow.Add(time.Minute)
	tests := []struct {
		name     string
		status   metav1.ConditionStatus
		reason   string
		wantTime time.Time
	}{
		{"same status keeps transition time", metav1.ConditionTrue, reasonReconciled, testNow},
		{"flip uses injected clock", metav1.ConditionFalse, reasonListNodesFailed, later},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := baseStatus()
			setReady(st, 2, tt.status, tt.reason, "msg", later)
			if len(st.Conditions) != 1 {
				t.Fatalf("want 1 condition, got %d", len(st.Conditions))
			}
			c := st.Conditions[0]
			if c.Status != tt.status || c.Reason != tt.reason || c.ObservedGeneration != 2 {
				t.Errorf("condition = %+v", c)
			}
			if !c.LastTransitionTime.Time.Equal(tt.wantTime) {
				t.Errorf("lastTransitionTime = %v, want %v", c.LastTransitionTime, tt.wantTime)
			}
		})
	}

	st := &observev1alpha1.ClusterViewStatus{}
	setReady(st, 1, metav1.ConditionTrue, reasonReconciled, "msg", later)
	if !st.Conditions[0].LastTransitionTime.Time.Equal(later) {
		t.Errorf("new condition should use injected clock, got %v", st.Conditions[0].LastTransitionTime)
	}
}

func TestDecide(t *testing.T) {
	const interval = 10 * time.Second
	tests := []struct {
		name        string
		lastWrite   time.Time
		interval    time.Duration
		changed     bool
		specChanged bool
		wantWrite   bool
		wantAfter   time.Duration
	}{
		{"unchanged never writes", testNow.Add(-time.Hour), interval, false, false, false, backstopInterval},
		{"unchanged even with spec change", testNow, interval, false, true, false, backstopInterval},
		{"first write", time.Time{}, interval, true, false, true, backstopInterval},
		{"inside window defers", testNow.Add(-4 * time.Second), interval, true, false, false, 6 * time.Second},
		{"exactly at interval writes", testNow.Add(-interval), interval, true, false, true, backstopInterval},
		{"past interval writes", testNow.Add(-time.Minute), interval, true, false, true, backstopInterval},
		{"remaining capped at backstop", testNow.Add(-time.Minute), 10 * time.Minute, true, false, false, backstopInterval},
		{"future lastWrite capped at interval", testNow.Add(time.Hour), interval, true, false, false, interval},
		{"zero interval falls back to default", testNow.Add(-4 * time.Second), 0, true, false, false, 6 * time.Second},
		{"spec change bypasses window", testNow.Add(-time.Second), interval, true, true, true, backstopInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			write, after := decide(testNow, tt.lastWrite, tt.interval, tt.changed, tt.specChanged)
			if write != tt.wantWrite || after != tt.wantAfter {
				t.Errorf("decide() = (%v, %v), want (%v, %v)", write, after, tt.wantWrite, tt.wantAfter)
			}
		})
	}
}

func TestLastWriteTime(t *testing.T) {
	earlier, later := testNow, testNow.Add(time.Second)
	tests := []struct {
		name        string
		lastUpdated *metav1.Time
		tracked     time.Time
		want        time.Time
	}{
		{"neither", nil, time.Time{}, time.Time{}},
		{"status only (after restart)", &metav1.Time{Time: earlier}, time.Time{}, earlier},
		{"tracker ahead of stale cache", &metav1.Time{Time: earlier}, later, later},
		{"status ahead of tracker", &metav1.Time{Time: later}, earlier, later},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lastWriteTime(tt.lastUpdated, tt.tracked); !got.Equal(tt.want) {
				t.Errorf("lastWriteTime() = %v, want %v", got, tt.want)
			}
		})
	}
}
