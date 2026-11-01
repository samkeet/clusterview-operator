package controller

import (
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
	"github.com/samkeet/clusterview-operator/internal/clusterstate"
)

const (
	// defaultRefreshInterval matches the CRD default. It only applies if defaulting
	// was bypassed, so a zero interval can't turn the throttle off.
	defaultRefreshInterval = 10 * time.Second
	// backstopInterval re-reads the cache even when no event arrives, in case one was
	// missed. It's a cache read, so it costs the API server nothing unless state changed.
	backstopInterval = 5 * time.Minute
	// conflictRetry is short because a 409 only means our cached base was stale. The
	// cache normally catches up within milliseconds.
	conflictRetry = time.Second
)

// Ready condition. Reasons are CamelCase per metav1.Condition. Messages are fixed
// strings so a repeated failure doesn't count as a content change and cause a write.
const (
	conditionReady        = "Ready"
	reasonReconciled      = "Reconciled"
	reasonListNodesFailed = "ListNodesFailed"
	reasonListPodsFailed  = "ListPodsFailed"
)

// applySnapshot copies the aggregated counts into st. Empty collections become nil
// to match what the API server returns for omitempty fields.
func applySnapshot(st *observev1alpha1.ClusterViewStatus, snap clusterstate.Snapshot) {
	st.NodeCount = snap.Summary.NodeCount
	st.ReadyNodes = snap.Summary.ReadyNodes
	st.NotReadyNodes = snap.Summary.NotReadyNodes
	st.PodCount = snap.Summary.PodCount

	st.PodPhases = nil
	if len(snap.Summary.PodPhases) > 0 {
		st.PodPhases = snap.Summary.PodPhases
	}

	st.Nodes = nil
	if len(snap.Nodes) > 0 {
		st.Nodes = make([]observev1alpha1.NodeSummary, 0, len(snap.Nodes))
		for _, n := range snap.Nodes {
			st.Nodes = append(st.Nodes, observev1alpha1.NodeSummary{
				Name:     n.Name,
				Status:   n.Status,
				PodCount: n.PodCount,
			})
		}
	}
}

// setReady sets the Ready condition. The transition time is passed in explicitly
// because SetStatusCondition would otherwise call time.Now(), bypassing the
// injected clock. It keeps the existing time when the status doesn't flip.
func setReady(st *observev1alpha1.ClusterViewStatus, generation int64,
	status metav1.ConditionStatus, reason, message string, now time.Time) {
	apimeta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type:               conditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
		LastTransitionTime: metav1.NewTime(now),
	})
}

// contentEqual reports whether two statuses differ only in timestamps. Timestamps
// are written as a side effect of a write, so counting them as content would make
// every reconcile look like a change.
func contentEqual(a, b *observev1alpha1.ClusterViewStatus) bool {
	return equality.Semantic.DeepEqual(withoutTimestamps(a), withoutTimestamps(b))
}

func withoutTimestamps(st *observev1alpha1.ClusterViewStatus) *observev1alpha1.ClusterViewStatus {
	c := st.DeepCopy()
	c.LastUpdated = nil
	for i := range c.Conditions {
		c.Conditions[i].LastTransitionTime = metav1.Time{}
	}
	return c
}

// decide applies the write throttle. refreshInterval is the minimum gap between
// writes, so a pending change is deferred, not dropped. The caller requeues after
// the returned duration, so a trailing write is guaranteed. The returned duration
// is never above the backstop, so every CR is re-checked at least every 5m.
func decide(now, lastWrite time.Time, interval time.Duration, changed, specChanged bool) (bool, time.Duration) {
	if !changed {
		return false, backstopInterval
	}
	if interval <= 0 {
		interval = defaultRefreshInterval
	}
	// Spec edits are rare and made by a person, who shouldn't have to wait up to 10m.
	if specChanged || lastWrite.IsZero() {
		return true, backstopInterval
	}
	elapsed := now.Sub(lastWrite)
	if elapsed >= interval {
		return true, backstopInterval
	}
	// The interval cap handles a lastWrite in the future (clock skew), which would
	// otherwise defer the write for longer than one window.
	return false, min(interval-elapsed, interval, backstopInterval)
}

// lastWriteTime is the later of the persisted lastUpdated (survives restarts) and
// the in-memory record (ahead of the cache right after our own write).
func lastWriteTime(lastUpdated *metav1.Time, tracked time.Time) time.Time {
	if lastUpdated != nil && lastUpdated.After(tracked) {
		return lastUpdated.Time
	}
	return tracked
}
