package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
	"github.com/samkeet/clusterview-operator/internal/clusterstate"
)

const (
	cvName      = "cluster"
	pathSummary = "/api/v1/summary"
	pathNodes   = "/api/v1/nodes"
	pathN1Pods  = "/api/v1/nodes/n1/pods"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := observev1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// newHandler builds the real routes over a fake client that has the same field
// indexes main.go registers on the cache, so MatchingFields behaves as in production.
func newHandler(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) http.Handler {
	t.Helper()
	b := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithIndex(&corev1.Pod{}, PodNodeNameField, PodNodeName).
		WithIndex(&eventsv1.Event{}, EventRegardingField, EventRegarding).
		WithObjects(objs...)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	s := &Server{Reader: b.Build(), ClusterViewName: cvName, Log: logr.Discard()}
	return s.Handler()
}

func readyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func testPod(ns, name, node string, phase corev1.PodPhase, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase:             phase,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "c", RestartCount: restarts}},
		},
	}
}

func clusterView(namespaces ...string) *observev1alpha1.ClusterView {
	return &observev1alpha1.ClusterView{
		ObjectMeta: metav1.ObjectMeta{Name: cvName},
		Spec:       observev1alpha1.ClusterViewSpec{Namespaces: namespaces},
	}
}

func get(t *testing.T, h http.Handler, path string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return v
}

// sampleObjects: two nodes, pods in two namespaces, one unscheduled pod.
func sampleObjects() []client.Object {
	return []client.Object{
		readyNode("n1"), readyNode("n2"),
		testPod("a", "web-1", "n1", corev1.PodRunning, 3),
		testPod("a", "web-0", "n1", corev1.PodRunning, 0),
		testPod("b", "db-0", "n1", corev1.PodPending, 0),
		testPod("b", "job", "n2", corev1.PodSucceeded, 0),
		testPod("a", "pending", "", corev1.PodPending, 0),
	}
}

func TestSummary(t *testing.T) {
	rec := get(t, newHandler(t, nil, sampleObjects()...), pathSummary)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	got := decode[clusterstate.Summary](t, rec)
	want := clusterstate.Summary{
		NodeCount: 2, ReadyNodes: 2, PodCount: 5, UnscheduledPods: 1,
		PodPhases: map[string]int32{"Running": 2, "Pending": 2, "Succeeded": 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
}

func TestNodesReturnsSnapshotWithoutPods(t *testing.T) {
	rec := get(t, newHandler(t, nil, sampleObjects()...), pathNodes)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	// The contract is {"summary": ..., "nodes": [...]} with no pod lists anywhere.
	raw := decode[map[string]json.RawMessage](t, rec)
	if len(raw) != 2 || raw["summary"] == nil || raw["nodes"] == nil {
		t.Fatalf("top-level keys = %v, want summary and nodes", keys(raw))
	}
	if strings.Contains(rec.Body.String(), `"pods":[`) {
		t.Errorf("nodes payload must not include pod lists: %s", rec.Body)
	}
	snap := decode[clusterstate.Snapshot](t, rec)
	if snap.Summary.PodCount != 5 {
		t.Errorf("summary.podCount = %d, want 5", snap.Summary.PodCount)
	}
	counts := map[string]int32{}
	for _, n := range snap.Nodes {
		counts[n.Name] = n.PodCount
	}
	if want := map[string]int32{"n1": 3, "n2": 1}; !reflect.DeepEqual(counts, want) {
		t.Errorf("per-node pod counts = %v, want %v", counts, want)
	}
}

func TestNodesEmptyClusterIsArrayNotNull(t *testing.T) {
	rec := get(t, newHandler(t, nil), pathNodes)
	raw := decode[map[string]json.RawMessage](t, rec)
	if string(raw["nodes"]) != "[]" {
		t.Errorf("nodes = %s, want []", raw["nodes"])
	}
}

func TestNodePods(t *testing.T) {
	rec := get(t, newHandler(t, nil, sampleObjects()...), pathN1Pods)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	got := decode[nodePodsResponse](t, rec)
	if got.Node != "n1" {
		t.Errorf("node = %q", got.Node)
	}
	// Only n1's pods, sorted by namespace then name.
	names := make([]string, 0, len(got.Pods))
	for _, p := range got.Pods {
		names = append(names, p.Namespace+"/"+p.Name)
	}
	if want := []string{"a/web-0", "a/web-1", "b/db-0"}; !reflect.DeepEqual(names, want) {
		t.Errorf("pods = %v, want %v", names, want)
	}
	if got.Pods[1].Restarts != 3 || got.Pods[2].Phase != corev1.PodPending {
		t.Errorf("restarts/phase not carried through: %+v", got.Pods)
	}
}

func TestNodePodsEmptyNodeIsArrayNotNull(t *testing.T) {
	rec := get(t, newHandler(t, nil, readyNode("idle")), "/api/v1/nodes/idle/pods")
	raw := decode[map[string]json.RawMessage](t, rec)
	if string(raw["pods"]) != "[]" {
		t.Errorf("pods = %s, want []", raw["pods"])
	}
}

func TestNodePodsUnknownNode(t *testing.T) {
	rec := get(t, newHandler(t, nil, sampleObjects()...), "/api/v1/nodes/nope/pods")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if e := decode[errorResponse](t, rec); !strings.Contains(e.Error, "nope") {
		t.Errorf("error = %q", e.Error)
	}
}

func TestNamespaceFilterFromClusterView(t *testing.T) {
	h := newHandler(t, nil, append(sampleObjects(), clusterView("a"))...)

	sum := decode[clusterstate.Summary](t, get(t, h, pathSummary))
	if sum.PodCount != 3 {
		t.Errorf("summary.podCount = %d, want 3 (namespace a only)", sum.PodCount)
	}
	snap := decode[clusterstate.Snapshot](t, get(t, h, pathNodes))
	if snap.Nodes[0].PodCount != 2 || snap.Nodes[1].PodCount != 0 {
		t.Errorf("filtered per-node counts = %d, %d; want 2, 0", snap.Nodes[0].PodCount, snap.Nodes[1].PodCount)
	}
	pods := decode[nodePodsResponse](t, get(t, h, pathN1Pods))
	for _, p := range pods.Pods {
		if p.Namespace != "a" {
			t.Errorf("pod %s/%s leaked through the namespace filter", p.Namespace, p.Name)
		}
	}
	if len(pods.Pods) != 2 {
		t.Errorf("filtered node pods = %d, want 2", len(pods.Pods))
	}
}

func TestClusterViewErrorFailsClosed(t *testing.T) {
	// If the filter can't be read, showing all namespaces would expose pods the
	// user excluded, so the request must fail instead.
	funcs := &interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*observev1alpha1.ClusterView); ok {
				return errors.New("cache unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	h := newHandler(t, funcs, sampleObjects()...)
	for _, path := range []string{pathSummary, pathNodes, pathN1Pods} {
		rec := get(t, h, path)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: status = %d, want 500", path, rec.Code)
		}
		if e := decode[errorResponse](t, rec); e.Error != "internal error" {
			t.Errorf("%s: error = %q, want the generic message", path, e.Error)
		}
	}
}

func TestETag(t *testing.T) {
	h := newHandler(t, nil, sampleObjects()...)
	for _, path := range []string{pathSummary, pathNodes, pathN1Pods} {
		t.Run(path, func(t *testing.T) {
			first := get(t, h, path)
			etag := first.Header().Get("ETag")
			if !strings.HasPrefix(etag, `"`) || len(etag) != 34 {
				t.Fatalf("ETag = %q, want a quoted 32-hex strong tag", etag)
			}
			tests := []struct {
				name        string
				ifNoneMatch string
				want        int
			}{
				{"matching", etag, http.StatusNotModified},
				{"weakened by a proxy", "W/" + etag, http.StatusNotModified},
				{"in a list", `"other", ` + etag, http.StatusNotModified},
				{"different", `"stale"`, http.StatusOK},
			}
			for _, tt := range tests {
				rec := get(t, h, path, "If-None-Match", tt.ifNoneMatch)
				if rec.Code != tt.want {
					t.Errorf("%s: status = %d, want %d", tt.name, rec.Code, tt.want)
				}
				if tt.want == http.StatusNotModified && rec.Body.Len() != 0 {
					t.Errorf("%s: 304 must have an empty body, got %q", tt.name, rec.Body)
				}
			}
		})
	}
}

func TestETagChangesWithNamespaceFilter(t *testing.T) {
	all := get(t, newHandler(t, nil, sampleObjects()...), pathSummary).Header().Get("ETag")
	filtered := get(t, newHandler(t, nil, append(sampleObjects(), clusterView("a"))...), pathSummary).Header().Get("ETag")
	if all == filtered {
		t.Errorf("ETag must change when the filter changes the body")
	}
}

func TestNonGETMethodsRejected(t *testing.T) {
	h := newHandler(t, nil, sampleObjects()...)
	paths := []string{pathSummary, pathNodes, pathN1Pods, "/api/v1/namespaces/a/pods/web-0/events", "/healthz"}
	methods := []string{http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions}
	for _, path := range paths {
		for _, m := range methods {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(m, path, nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: status = %d, want 405", m, path, rec.Code)
			}
			if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
				t.Errorf("%s %s: Allow = %q, want GET", m, path, allow)
			}
		}
	}
}

func TestHealthz(t *testing.T) {
	rec := get(t, newHandler(t, nil), "/healthz")
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("healthz = %d %q", rec.Code, rec.Body)
	}
}

func TestSecurityHeaders(t *testing.T) {
	rec := get(t, newHandler(t, nil), pathSummary)
	for header, want := range map[string]string{
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "default-src 'self'; frame-ancestors 'none'",
		"X-Frame-Options":         "DENY",
		"Referrer-Policy":         "no-referrer",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("no CORS header expected, got %q", got)
	}
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
