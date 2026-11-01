package controller

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// lastWrite remembers our own most recent status write. The UID guards against a
// ClusterView that was deleted and recreated under the same name inheriting the old
// throttle window when the delete was never observed as NotFound.
type lastWrite struct {
	at  time.Time
	uid types.UID
}

// writeTracker covers informer-cache lag right after our own patch: until the cache
// shows the new status.lastUpdated, this is the only record that a write happened.
// The zero value is ready to use so the reconciler needs no constructor.
type writeTracker struct {
	mu sync.Mutex
	m  map[string]lastWrite
}

// get returns the last write time for name, or the zero time if there is none or it
// belongs to a different object with the same name.
func (t *writeTracker) get(name string, uid types.UID) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	w, ok := t.m[name]
	if !ok || w.uid != uid {
		return time.Time{}
	}
	return w.at
}

func (t *writeTracker) record(name string, uid types.UID, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil {
		t.m = map[string]lastWrite{}
	}
	t.m[name] = lastWrite{at: at, uid: uid}
}

func (t *writeTracker) forget(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.m, name)
}
