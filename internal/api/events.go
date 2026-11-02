package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/samkeet/clusterview-operator/internal/clusterstate"
)

// The API reads Events from the cache, which only needs list/watch. The marker
// lives here, next to the only code that reads Events; the controller never does.
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=list;watch

const (
	defaultEventLimit = 5
	maxEventLimit     = 50
)

// EventInfo is one event as the drawer shows it. Name is the Event's own name,
// a stable key so the UI can keep an expanded row expanded across re-renders.
type EventInfo struct {
	Name     string    `json:"name"`
	Type     string    `json:"type"`
	Reason   string    `json:"reason"`
	Message  string    `json:"message"`
	Count    int32     `json:"count"`
	LastSeen time.Time `json:"lastSeen"`
}

// podEventsResponse is one page of a pod's events, newest first. Total is the
// number of events across all pages, for "1–5 of N".
type podEventsResponse struct {
	Total  int         `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
	Events []EventInfo `json:"events"`
}

func (s *Server) podEvents(w http.ResponseWriter, r *http.Request) {
	ns, name := r.PathValue("ns"), r.PathValue("name")
	limit, offset, err := parsePage(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	namespaces, err := s.namespaces(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// A namespace the user filtered out doesn't exist as far as the API goes.
	if !clusterstate.InNamespaces(ns, namespaces) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	events, err := s.eventsForPod(r.Context(), ns, name)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	lo, hi := paginate(len(events), limit, offset)
	page := make([]EventInfo, 0, hi-lo)
	for i := lo; i < hi; i++ {
		page = append(page, newEventInfo(&events[i]))
	}
	s.writeJSON(w, r, podEventsResponse{Total: len(events), Limit: limit, Offset: offset, Events: page})
}

// eventsForPod returns the pod's events from the index, newest first. The pod
// doesn't have to exist: events outlive a deleted pod, and that's when they're
// most useful. If it is cached, only events for its UID are kept, so events from
// an earlier pod with the same name (a StatefulSet's web-0) don't leak in.
func (s *Server) eventsForPod(ctx context.Context, ns, name string) ([]eventsv1.Event, error) {
	uid, err := s.podUID(ctx, ns, name)
	if err != nil {
		return nil, err
	}
	var list eventsv1.EventList
	// Read-only use, so skip deep-copying cached objects.
	if err := s.Reader.List(ctx, &list, client.InNamespace(ns), client.UnsafeDisableDeepCopy,
		client.MatchingFields{EventRegardingField: RegardingKey("Pod", ns, name)}); err != nil {
		return nil, fmt.Errorf("listing events for pod %s/%s: %w", ns, name, err)
	}
	// The slice is ours (only its elements share memory with the cache), so
	// filtering and sorting it in place doesn't touch cached objects.
	events := slices.DeleteFunc(list.Items, func(e eventsv1.Event) bool {
		return uid != "" && e.Regarding.UID != "" && e.Regarding.UID != uid
	})
	sortEvents(events)
	return events, nil
}

// podUID returns the cached pod's UID, or "" if the pod isn't in the cache.
func (s *Server) podUID(ctx context.Context, ns, name string) (types.UID, error) {
	var pod corev1.Pod
	err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &pod, client.UnsafeDisableDeepCopy)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("getting pod %s/%s: %w", ns, name, err)
	}
	return pod.UID, nil
}

// parsePage reads limit (1..50, default 5) and offset (>= 0, default 0). A
// parameter that's present must be a valid integer: "limit=" is an error, not
// the default, so a broken client finds out.
func parsePage(q url.Values) (limit, offset int, err error) {
	limit, offset = defaultEventLimit, 0
	if q.Has("limit") {
		v, err := strconv.ParseInt(q.Get("limit"), 10, 32)
		if err != nil || v < 1 || v > maxEventLimit {
			return 0, 0, fmt.Errorf("limit must be an integer from 1 to %d", maxEventLimit)
		}
		limit = int(v)
	}
	if q.Has("offset") {
		v, err := strconv.ParseInt(q.Get("offset"), 10, 32)
		if err != nil || v < 0 {
			return 0, 0, errors.New("offset must be a non-negative integer")
		}
		offset = int(v)
	}
	return limit, offset, nil
}

// paginate returns the [lo, hi) slice bounds for one page of n items. An offset
// past the end gives an empty page, not an error, so a client on a page that
// shrank (events expire) gets the real total and can move to the last page.
func paginate(n, limit, offset int) (lo, hi int) {
	lo = min(offset, n)
	return lo, min(lo+limit, n)
}

// sortEvents orders events newest first by lastSeen, then by when the event was
// first recorded, then by name. The name tie-break makes the order total, so
// pages and ETags are stable across polls.
func sortEvents(events []eventsv1.Event) {
	slices.SortFunc(events, func(a, b eventsv1.Event) int {
		return cmp.Or(
			eventLastSeen(&b).Compare(eventLastSeen(&a)),
			eventFirstSeen(&b).Compare(eventFirstSeen(&a)),
			strings.Compare(a.Name, b.Name),
		)
	})
}

func newEventInfo(e *eventsv1.Event) EventInfo {
	return EventInfo{
		Name:     e.Name,
		Type:     e.Type,
		Reason:   e.Reason,
		Message:  e.Note,
		Count:    eventCount(e),
		LastSeen: eventLastSeen(e).UTC(),
	}
}

// eventLastSeen is the most recent time the event was observed. The kubelet
// still writes through the legacy recorder (no eventTime, only the deprecated
// timestamps), and a repeating new-API event keeps its latest time in series,
// so this takes the first time that's set:
// series.lastObservedTime, deprecatedLastTimestamp, eventTime,
// deprecatedFirstTimestamp, creationTimestamp.
func eventLastSeen(e *eventsv1.Event) time.Time {
	if e.Series != nil && !e.Series.LastObservedTime.IsZero() {
		return e.Series.LastObservedTime.Time
	}
	if !e.DeprecatedLastTimestamp.IsZero() {
		return e.DeprecatedLastTimestamp.Time
	}
	return eventFirstSeen(e)
}

// eventFirstSeen is when the event was first recorded: eventTime,
// deprecatedFirstTimestamp, then creationTimestamp.
func eventFirstSeen(e *eventsv1.Event) time.Time {
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	if !e.DeprecatedFirstTimestamp.IsZero() {
		return e.DeprecatedFirstTimestamp.Time
	}
	return e.CreationTimestamp.Time
}

// eventCount matches kubectl: series.count, then deprecatedCount, then 1.
// client-go's new-API recorder starts series.count at 1 on the first repeat, so
// such events can show one fewer than the true number; we don't guess.
func eventCount(e *eventsv1.Event) int32 {
	if e.Series != nil && e.Series.Count > 0 {
		return e.Series.Count
	}
	if e.DeprecatedCount > 0 {
		return e.DeprecatedCount
	}
	return 1
}
