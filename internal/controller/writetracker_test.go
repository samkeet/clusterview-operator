package controller

import (
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

func TestWriteTracker(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		setup func(*writeTracker)
		uid   types.UID
		want  time.Time
	}{
		{
			name:  "zero value has no entry",
			setup: func(*writeTracker) {},
			uid:   "a",
			want:  time.Time{},
		},
		{
			name:  "recorded write is returned",
			setup: func(w *writeTracker) { w.record("cv", "a", t0) },
			uid:   "a",
			want:  t0,
		},
		{
			name:  "later record replaces earlier",
			setup: func(w *writeTracker) { w.record("cv", "a", t0); w.record("cv", "a", t0.Add(time.Minute)) },
			uid:   "a",
			want:  t0.Add(time.Minute),
		},
		{
			name:  "recreated object with new UID starts fresh",
			setup: func(w *writeTracker) { w.record("cv", "a", t0) },
			uid:   "b",
			want:  time.Time{},
		},
		{
			name:  "forget drops the entry",
			setup: func(w *writeTracker) { w.record("cv", "a", t0); w.forget("cv") },
			uid:   "a",
			want:  time.Time{},
		},
		{
			name:  "forget on zero value is safe",
			setup: func(w *writeTracker) { w.forget("cv") },
			uid:   "a",
			want:  time.Time{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var w writeTracker
			tt.setup(&w)
			if got := w.get("cv", tt.uid); !got.Equal(tt.want) {
				t.Errorf("get() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Meaningful under `go test -race`: the tracker must stay safe if
// MaxConcurrentReconciles is ever raised.
func TestWriteTrackerConcurrent(t *testing.T) {
	var w writeTracker
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			name := string(rune('a' + i))
			for j := range 100 {
				w.record(name, "u", time.Unix(int64(j), 0))
				_ = w.get(name, "u")
				if j%10 == 0 {
					w.forget(name)
				}
			}
		})
	}
	wg.Wait()
}
