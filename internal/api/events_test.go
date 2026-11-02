package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func at(m int) metav1.Time { return metav1.NewTime(t0.Add(time.Duration(m) * time.Minute)) }

// legacyEvent is shaped like a kubelet event read through events.k8s.io: no
// eventTime and no series, only the deprecated timestamps and count.
func legacyEvent(ns, pod, name string, uid types.UID, lastMin int, count int32) *eventsv1.Event {
	return &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, CreationTimestamp: at(lastMin)},
		Regarding:  corev1.ObjectReference{Kind: kindPod, Namespace: ns, Name: pod, UID: uid},
		Type:       corev1.EventTypeNormal, Reason: "R-" + name, Note: "note " + name,
		DeprecatedFirstTimestamp: at(lastMin), DeprecatedLastTimestamp: at(lastMin),
		DeprecatedCount: count,
	}
}

func eventsPath(ns, pod, query string) string {
	p := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/events", ns, pod)
	if query != "" {
		p += "?" + query
	}
	return p
}

func eventNames(r podEventsResponse) []string {
	out := make([]string, 0, len(r.Events))
	for _, e := range r.Events {
		out = append(out, e.Name)
	}
	return out
}

// sevenEvents: pod a/web-0 has e1..e7 (e7 newest), plus noise that must never show.
func sevenEvents() []client.Object {
	objs := []client.Object{testPod("a", "web-0", "n1", corev1.PodRunning, 0)}
	for i := 1; i <= 7; i++ {
		objs = append(objs, legacyEvent("a", "web-0", fmt.Sprintf("e%d", i), "", i, 1))
	}
	objs = append(objs,
		legacyEvent("a", "web-1", "other-pod", "", 99, 1),
		legacyEvent("b", "web-0", "other-ns", "", 99, 1),
		&eventsv1.Event{
			ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "rs-event"},
			Regarding:  corev1.ObjectReference{Kind: "ReplicaSet", Namespace: "a", Name: "web-0"},
		},
	)
	return objs
}

func TestPodEventsPaging(t *testing.T) {
	h := newHandler(t, nil, sevenEvents()...)
	tests := []struct {
		query      string
		wantLimit  int
		wantOffset int
		wantNames  []string
	}{
		{query: "", wantLimit: 5, wantOffset: 0, wantNames: []string{"e7", "e6", "e5", "e4", "e3"}},
		{query: "limit=5&offset=5", wantLimit: 5, wantOffset: 5, wantNames: []string{"e2", "e1"}},
		{query: "limit=2&offset=1", wantLimit: 2, wantOffset: 1, wantNames: []string{"e6", "e5"}},
		{query: "limit=50", wantLimit: 50, wantOffset: 0, wantNames: []string{"e7", "e6", "e5", "e4", "e3", "e2", "e1"}},
		// Past the end: an empty page with the real total, not an error.
		{query: "offset=7", wantLimit: 5, wantOffset: 7, wantNames: []string{}},
		{query: "offset=1000", wantLimit: 5, wantOffset: 1000, wantNames: []string{}},
		// Unknown parameters are ignored.
		{query: "foo=bar", wantLimit: 5, wantOffset: 0, wantNames: []string{"e7", "e6", "e5", "e4", "e3"}},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			rec := get(t, h, eventsPath("a", "web-0", tt.query))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
			}
			got := decode[podEventsResponse](t, rec)
			if got.Total != 7 || got.Limit != tt.wantLimit || got.Offset != tt.wantOffset {
				t.Errorf("total/limit/offset = %d/%d/%d, want 7/%d/%d",
					got.Total, got.Limit, got.Offset, tt.wantLimit, tt.wantOffset)
			}
			if names := eventNames(got); !reflect.DeepEqual(names, tt.wantNames) {
				t.Errorf("events = %v, want %v", names, tt.wantNames)
			}
		})
	}
}

func TestPodEventsShape(t *testing.T) {
	ev := legacyEvent("a", "web-0", "e1", "", 3, 4)
	ev.Type = corev1.EventTypeWarning
	h := newHandler(t, nil, ev)
	rec := get(t, h, eventsPath("a", "web-0", ""))
	// Exact keys, so a contract change can't slip in unnoticed.
	raw := decode[map[string]json.RawMessage](t, rec)
	if len(raw) != 4 || raw["total"] == nil || raw["limit"] == nil || raw["offset"] == nil || raw["events"] == nil {
		t.Fatalf("top-level keys = %v", keys(raw))
	}
	items := decode[struct {
		Events []map[string]json.RawMessage `json:"events"`
	}](t, rec).Events
	if len(items) != 1 || len(items[0]) != 6 {
		t.Fatalf("events = %v, want one item with 6 keys", items)
	}
	got := decode[podEventsResponse](t, rec).Events[0]
	want := EventInfo{Name: "e1", Type: "Warning", Reason: "R-e1", Message: "note e1", Count: 4, LastSeen: t0.Add(3 * time.Minute)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("event = %+v, want %+v", got, want)
	}
}

func TestPodEventsEmptyIsArrayNotNull(t *testing.T) {
	// An unknown pod is not an error: events can outlive a pod, and a pod with
	// none simply has none.
	rec := get(t, newHandler(t, nil), eventsPath("a", "nope", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	raw := decode[map[string]json.RawMessage](t, rec)
	if string(raw["events"]) != "[]" || string(raw["total"]) != "0" {
		t.Errorf("events = %s, total = %s; want [] and 0", raw["events"], raw["total"])
	}
}

func TestPodEventsUIDFilter(t *testing.T) {
	current := testPod("a", "web-0", "n1", corev1.PodRunning, 0)
	current.UID = "uid-new"
	objs := []client.Object{
		legacyEvent("a", "web-0", "old", "uid-old", 1, 1),
		legacyEvent("a", "web-0", "new", "uid-new", 2, 1),
		legacyEvent("a", "web-0", "no-uid", "", 3, 1),
	}

	t.Run("pod cached: only its UID (and events with no UID)", func(t *testing.T) {
		got := decode[podEventsResponse](t, get(t, newHandler(t, nil, append(objs, current)...), eventsPath("a", "web-0", "")))
		if names := eventNames(got); !reflect.DeepEqual(names, []string{"no-uid", "new"}) || got.Total != 2 {
			t.Errorf("events = %v (total %d), want [no-uid new] (total 2)", names, got.Total)
		}
	})
	t.Run("pod deleted: every event for the name", func(t *testing.T) {
		got := decode[podEventsResponse](t, get(t, newHandler(t, nil, objs...), eventsPath("a", "web-0", "")))
		if names := eventNames(got); !reflect.DeepEqual(names, []string{"no-uid", "new", "old"}) {
			t.Errorf("events = %v, want [no-uid new old]", names)
		}
	})
}

func TestPodEventsBadParams(t *testing.T) {
	h := newHandler(t, nil, sevenEvents()...)
	for _, q := range []string{
		"limit=0", "limit=51", "limit=-1", "limit=abc", "limit=", "limit=1e3", "limit=2.5",
		"offset=-1", "offset=x", "offset=", "offset=99999999999",
	} {
		rec := get(t, h, eventsPath("a", "web-0", q))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, rec.Code)
			continue
		}
		if e := decode[errorResponse](t, rec); !strings.Contains(e.Error, "must be") {
			t.Errorf("%s: error = %q", q, e.Error)
		}
	}
}

func TestPodEventsNamespaceFilter(t *testing.T) {
	h := newHandler(t, nil, append(sevenEvents(), clusterView("a"))...)

	inside := get(t, h, eventsPath("a", "web-0", ""))
	if inside.Code != http.StatusOK || decode[podEventsResponse](t, inside).Total != 7 {
		t.Errorf("namespace in the filter: status %d, body %s", inside.Code, inside.Body)
	}
	// b holds a real event for b/web-0, but b is filtered out.
	outside := get(t, h, eventsPath("b", "web-0", ""))
	if outside.Code != http.StatusNotFound {
		t.Fatalf("namespace outside the filter: status = %d, want 404", outside.Code)
	}
	if e := decode[errorResponse](t, outside); e.Error != "not found" {
		t.Errorf("error = %q, want a plain not-found", e.Error)
	}
	if strings.Contains(outside.Body.String(), "other-ns") {
		t.Errorf("filtered event leaked: %s", outside.Body)
	}
}

func TestPodEventsReadErrorsFailClosed(t *testing.T) {
	boom := errors.New("cache unavailable")
	tests := map[string]*interceptor.Funcs{
		"ClusterView read": {Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*observev1alpha1.ClusterView); ok {
				return boom
			}
			return c.Get(ctx, key, obj, opts...)
		}},
		"pod read": {Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.Pod); ok {
				return boom
			}
			return c.Get(ctx, key, obj, opts...)
		}},
		"event list": {List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*eventsv1.EventList); ok {
				return boom
			}
			return c.List(ctx, list, opts...)
		}},
	}
	for name, funcs := range tests {
		t.Run(name, func(t *testing.T) {
			rec := get(t, newHandler(t, funcs, sevenEvents()...), eventsPath("a", "web-0", ""))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			if e := decode[errorResponse](t, rec); e.Error != "internal error" {
				t.Errorf("error = %q, want the generic message", e.Error)
			}
		})
	}
}

func TestPodEventsETag(t *testing.T) {
	path := eventsPath("a", "web-0", "")
	before := get(t, newHandler(t, nil, sevenEvents()...), path)
	etag := before.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	h := newHandler(t, nil, sevenEvents()...)
	if rec := get(t, h, path, "If-None-Match", etag); rec.Code != http.StatusNotModified {
		t.Errorf("unchanged: status = %d, want 304", rec.Code)
	}
	// A new event changes page 1 and the total, so the tag must change.
	newer := newHandler(t, nil, append(sevenEvents(), legacyEvent("a", "web-0", "e8", "", 8, 1))...)
	if rec := get(t, newer, path, "If-None-Match", etag); rec.Code != http.StatusOK {
		t.Errorf("after a new event: status = %d, want 200", rec.Code)
	}
}

func TestEventLastSeen(t *testing.T) {
	micro := func(m int) metav1.MicroTime { return metav1.NewMicroTime(at(m).Time) }
	tests := []struct {
		name string
		ev   eventsv1.Event
		want time.Time
	}{
		{name: "series wins", ev: eventsv1.Event{
			Series:    &eventsv1.EventSeries{Count: 2, LastObservedTime: micro(9)},
			EventTime: micro(1), DeprecatedLastTimestamp: at(5),
		}, want: at(9).Time},
		{name: "legacy kubelet event", ev: eventsv1.Event{
			DeprecatedFirstTimestamp: at(1), DeprecatedLastTimestamp: at(5),
		}, want: at(5).Time},
		{name: "new-API single event", ev: eventsv1.Event{EventTime: micro(3)}, want: at(3).Time},
		{name: "series with no time falls through", ev: eventsv1.Event{
			Series: &eventsv1.EventSeries{Count: 2}, EventTime: micro(3),
		}, want: at(3).Time},
		{name: "only first timestamp", ev: eventsv1.Event{DeprecatedFirstTimestamp: at(2)}, want: at(2).Time},
		{name: "only creation time", ev: eventsv1.Event{
			ObjectMeta: metav1.ObjectMeta{CreationTimestamp: at(4)},
		}, want: at(4).Time},
		{name: "nothing set", ev: eventsv1.Event{}, want: time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := eventLastSeen(&tt.ev); !got.Equal(tt.want) {
				t.Errorf("eventLastSeen = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEventCount(t *testing.T) {
	tests := []struct {
		name string
		ev   eventsv1.Event
		want int32
	}{
		{name: "series", ev: eventsv1.Event{Series: &eventsv1.EventSeries{Count: 4}, DeprecatedCount: 9}, want: 4},
		{name: "deprecated", ev: eventsv1.Event{DeprecatedCount: 3}, want: 3},
		{name: "neither", ev: eventsv1.Event{}, want: 1},
		{name: "empty series falls through", ev: eventsv1.Event{Series: &eventsv1.EventSeries{}, DeprecatedCount: 2}, want: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := eventCount(&tt.ev); got != tt.want {
				t.Errorf("eventCount = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSortEvents(t *testing.T) {
	ev := func(name string, last, first int) eventsv1.Event {
		return eventsv1.Event{
			ObjectMeta:              metav1.ObjectMeta{Name: name},
			DeprecatedLastTimestamp: at(last), DeprecatedFirstTimestamp: at(first),
		}
	}
	events := []eventsv1.Event{
		ev("b", 5, 1), ev("old", 1, 1), ev("a", 5, 1), ev("newer-start", 5, 3), ev("newest", 9, 0),
	}
	sortEvents(events)
	got := make([]string, 0, len(events))
	for _, e := range events {
		got = append(got, e.Name)
	}
	// lastSeen desc, then first seen desc, then name asc.
	if want := []string{"newest", "newer-start", "a", "b", "old"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestParsePage(t *testing.T) {
	tests := []struct {
		query      string
		wantLimit  int
		wantOffset int
		wantErr    bool
	}{
		{query: "", wantLimit: 5},
		{query: "limit=1", wantLimit: 1},
		{query: "limit=50&offset=10", wantLimit: 50, wantOffset: 10},
		{query: "offset=0", wantLimit: 5},
		{query: "limit=0", wantErr: true},
		{query: "limit=51", wantErr: true},
		{query: "limit=", wantErr: true},
		{query: "offset=-5", wantErr: true},
		{query: "offset=2147483648", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, "/?"+tt.query, nil)
			limit, offset, err := parsePage(req.URL.Query())
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && (limit != tt.wantLimit || offset != tt.wantOffset) {
				t.Errorf("limit/offset = %d/%d, want %d/%d", limit, offset, tt.wantLimit, tt.wantOffset)
			}
		})
	}
}

func TestPaginate(t *testing.T) {
	tests := []struct {
		n, limit, offset int
		lo, hi           int
	}{
		{n: 0, limit: 5, offset: 0, lo: 0, hi: 0},
		{n: 7, limit: 5, offset: 0, lo: 0, hi: 5},
		{n: 7, limit: 5, offset: 5, lo: 5, hi: 7},
		{n: 7, limit: 5, offset: 7, lo: 7, hi: 7},
		{n: 7, limit: 5, offset: 100, lo: 7, hi: 7},
		{n: 10, limit: 5, offset: 5, lo: 5, hi: 10},
	}
	for _, tt := range tests {
		if lo, hi := paginate(tt.n, tt.limit, tt.offset); lo != tt.lo || hi != tt.hi {
			t.Errorf("paginate(%d, %d, %d) = [%d, %d), want [%d, %d)", tt.n, tt.limit, tt.offset, lo, hi, tt.lo, tt.hi)
		}
	}
}

// TestStripEventKeepsHandlerInputs: the transform must never remove anything the
// endpoint reads, or the drawer would silently change.
func TestStripEventKeepsHandlerInputs(t *testing.T) {
	stripped, _ := StripEvent(fullEvent())
	got, want := newEventInfo(stripped.(*eventsv1.Event)), newEventInfo(fullEvent())
	if got != want {
		t.Errorf("newEventInfo changed after transform: got %+v, want %+v", got, want)
	}
	if g, w := EventRegarding(stripped.(*eventsv1.Event)), EventRegarding(fullEvent()); !reflect.DeepEqual(g, w) {
		t.Errorf("index key changed after transform: got %v, want %v", g, w)
	}
	if stripped.(*eventsv1.Event).Regarding.UID != fullEvent().Regarding.UID {
		t.Errorf("transform dropped regarding.uid, which the UID filter reads")
	}
}
