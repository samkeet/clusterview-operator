//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
	"github.com/samkeet/clusterview-operator/internal/clusterstate"
)

// Pods scheduled by the real scheduler onto kwok nodes, which kwok runs.
func TestBaselineMatchesCluster(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	ns := newNamespace(t, "")
	const replicas = 12
	labels := map[string]string{"cv-it/app": "baseline"}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "baseline"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](replicas),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: appContainer, Image: pauseImage}}},
			},
		},
	}
	g.Expect(env.admin.Create(ctx, dep)).To(Succeed())

	g.Eventually(func(g Gomega) {
		var pods corev1.PodList
		g.Expect(env.admin.List(ctx, &pods, client.InNamespace(ns))).To(Succeed())
		g.Expect(pods.Items).To(HaveLen(replicas))
		g.Expect(pods.Items).To(HaveEach(HaveField("Status.Phase", corev1.PodRunning)))
	}).WithTimeout(time.Minute).WithPolling(poll).Should(Succeed())

	eventuallyConverged(t)

	// Every kwok node's pod list must match, and together they hold all replicas.
	var nodes corev1.NodeList
	g.Expect(env.admin.List(ctx, &nodes)).To(Succeed())
	ours := 0
	for _, n := range nodes.Items {
		var got struct {
			Node string                 `json:"node"`
			Pods []clusterstate.PodInfo `json:"pods"`
		}
		g.Expect(apiGet("/api/v1/nodes/"+n.Name+"/pods", &got)).To(Equal(http.StatusOK))
		want, err := expectedPodsOn(ctx, n.Name)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(got.Node).To(Equal(n.Name))
		g.Expect(got.Pods).To(Equal(want), "pods on node %s", n.Name)
		for _, p := range got.Pods {
			if p.Namespace == ns {
				ours++
			}
		}
	}
	g.Expect(ours).To(Equal(replicas))
	logUsage(t)
}

// Phases, restart counts (init + app) and unscheduled pods, with exact values
// asserted directly rather than only through the oracle.
func TestPodPhasesAndRestarts(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	ns := newNamespace(t, "")
	node := newManualNode(t)

	setPodStatus(t, bindPod(t, ns, "crashy", node), corev1.PodRunning, 1, 2)
	setPodStatus(t, bindPod(t, ns, "failed", node), corev1.PodFailed, 0, 0)
	setPodStatus(t, bindPod(t, ns, "done", node), corev1.PodSucceeded, 0, 0)
	bindPod(t, ns, "waiting", node) // no status set: Pending

	unscheduled := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "unschedulable"},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"cv-it/no-such-node": "yes"},
			Containers:   []corev1.Container{{Name: appContainer, Image: pauseImage}},
		},
	}
	g.Expect(env.admin.Create(ctx, unscheduled)).To(Succeed())

	eventuallyConverged(t)

	var got struct {
		Pods []clusterstate.PodInfo `json:"pods"`
	}
	g.Expect(apiGet("/api/v1/nodes/"+node+"/pods", &got)).To(Equal(http.StatusOK))
	byName := map[string]clusterstate.PodInfo{}
	for _, p := range got.Pods {
		byName[p.Name] = p
	}
	g.Expect(byName).To(HaveLen(4))
	g.Expect(byName["crashy"]).To(And(HaveField("Phase", corev1.PodRunning), HaveField("Restarts", int32(3))))
	g.Expect(byName["failed"]).To(HaveField("Phase", corev1.PodFailed))
	g.Expect(byName["done"]).To(HaveField("Phase", corev1.PodSucceeded))
	g.Expect(byName["waiting"]).To(And(HaveField("Phase", corev1.PodPending), HaveField("Restarts", int32(0))))

	var sum clusterstate.Summary
	g.Expect(apiGet("/api/v1/summary", &sum)).To(Equal(http.StatusOK))
	g.Expect(sum.UnscheduledPods).To(BeNumerically(">=", 1))
	for _, phase := range []string{"Running", "Failed", "Succeeded", "Pending"} {
		g.Expect(sum.PodPhases).To(HaveKeyWithValue(phase, BeNumerically(">=", 1)))
	}
	logUsage(t)
}

// Ready -> NotReady -> Unknown -> deleted, as seen by both the CR and the API.
func TestNodeReadinessLifecycle(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	ns := newNamespace(t, "")
	node := newManualNode(t)
	bindPod(t, ns, "orphan-to-be", node)

	for _, step := range []struct {
		ready corev1.ConditionStatus
		want  observev1alpha1.NodeReadiness
	}{
		{corev1.ConditionTrue, observev1alpha1.NodeReady},
		{corev1.ConditionFalse, observev1alpha1.NodeNotReady},
		{"", observev1alpha1.NodeUnknown}, // Ready condition missing
		{corev1.ConditionUnknown, observev1alpha1.NodeUnknown},
	} {
		setNodeReady(t, node, step.ready)
		g.Eventually(func(g Gomega) {
			expectConverged(g)
			cv, err := getView(ctx)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(cv.Status.Nodes).To(ContainElement(observev1alpha1.NodeSummary{
				Name: node, Status: step.want, PodCount: 1,
			}))
		}).WithTimeout(converge).WithPolling(poll).Should(Succeed(), "Ready=%q", step.ready)
	}

	// Deleting the node leaves its pod bound to a missing node. Build still counts it
	// in the totals, and the node's pod endpoint 404s.
	before, err := expected(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(env.admin.Delete(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}})).To(Succeed())
	g.Eventually(func(g Gomega) {
		expectConverged(g)
		cv, err := getView(ctx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cv.Status.NodeCount).To(Equal(before.Summary.NodeCount - 1))
		g.Expect(cv.Status.PodCount).To(Equal(before.Summary.PodCount))
		g.Expect(apiGet("/api/v1/nodes/"+node+"/pods", nil)).To(Equal(http.StatusNotFound))
	}).WithTimeout(converge).WithPolling(poll).Should(Succeed())
	logUsage(t)
}

func TestNamespaceFilter(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	shown, hidden := newNamespace(t, "-a"), newNamespace(t, "-b")
	node := newManualNode(t)
	for i := range 3 {
		bindPod(t, shown, fmt.Sprintf("shown-%d", i), node)
	}
	for i := range 2 {
		bindPod(t, hidden, fmt.Sprintf("hidden-%d", i), node)
	}
	eventuallyConverged(t)

	setFilter(t, []string{shown})
	t.Cleanup(func() { setFilter(t, nil) })

	// A spec change bypasses the write throttle, so it lands within one interval.
	g.Eventually(func(g Gomega) {
		expectConverged(g)
		cv, err := getView(ctx)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(cv.Status.PodCount).To(Equal(int32(3)))
	}).WithTimeout(refresh - time.Second).WithPolling(poll).Should(Succeed())

	var got struct {
		Pods []clusterstate.PodInfo `json:"pods"`
	}
	g.Expect(apiGet("/api/v1/nodes/"+node+"/pods", &got)).To(Equal(http.StatusOK))
	g.Expect(got.Pods).To(HaveLen(3))
	g.Expect(got.Pods).To(HaveEach(HaveField("Namespace", shown)))
	logUsage(t)
}

// Under steady churn the controller writes at most once per refreshInterval, keeps
// writing during the churn (no starvation) and makes a trailing write at the end.
func TestStatusWriteThrottle(t *testing.T) {
	g := NewWithT(t)
	ns := newNamespace(t, "")
	node := newManualNode(t)
	eventuallyConverged(t)

	stop := watchStatusWrites(t)
	const churnFor = 4 * refresh
	tick := time.NewTicker(250 * time.Millisecond)
	for i, end := 0, time.Now().Add(churnFor); time.Now().Before(end); i++ {
		<-tick.C
		bindPod(t, ns, fmt.Sprintf("churn-%03d", i), node)
	}
	tick.Stop()
	eventuallyConverged(t) // the trailing write
	times := stop()

	t.Logf("%d status writes during %s of churn: %v", len(times), churnFor, gaps(times))
	g.Expect(len(times)).To(BeNumerically(">=", 3), "status must keep updating during churn")
	g.Expect(len(times)).To(BeNumerically("<=", int(churnFor/refresh)+2), "throttle exceeded")
	for _, gap := range gaps(times) {
		// Watch delivery jitter is a few ms; the throttle itself is exact.
		g.Expect(gap).To(BeNumerically(">=", refresh-300*time.Millisecond), "writes too close together")
	}
	logUsage(t)
}

// With nothing changing (kwok heartbeats keep updating nodes), there are no writes.
func TestIdleClusterWritesNothing(t *testing.T) {
	eventuallyQuiet(t)
	stop := watchStatusWrites(t)
	time.Sleep(2*refresh + 2*time.Second) // observing a quiet window, not waiting on a condition
	if times := stop(); len(times) != 0 {
		t.Fatalf("expected no status writes on an idle cluster, got %d", len(times))
	}
	logUsage(t)
}

func TestAPIContract(t *testing.T) {
	g := NewWithT(t)

	notGet := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead}
	for _, method := range notGet {
		req, err := http.NewRequest(method, env.apiURL+"/api/v1/nodes", nil)
		g.Expect(err).NotTo(HaveOccurred())
		resp, err := env.http.Do(req)
		g.Expect(err).NotTo(HaveOccurred())
		_ = resp.Body.Close()
		g.Expect(resp.StatusCode).To(Equal(http.StatusMethodNotAllowed), method)
		g.Expect(resp.Header.Get("Allow")).To(Equal(http.MethodGet), method)
	}

	resp, err := env.http.Get(env.apiURL + "/api/v1/nodes")
	g.Expect(err).NotTo(HaveOccurred())
	_ = resp.Body.Close()
	g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
	g.Expect(resp.Header.Get("Content-Type")).To(HavePrefix("application/json"))
	g.Expect(resp.Header.Get("Cache-Control")).To(Equal("no-cache"))
	g.Expect(resp.Header.Get("X-Content-Type-Options")).To(Equal("nosniff"))
	g.Expect(resp.Header.Get("X-Frame-Options")).To(Equal("DENY"))
	g.Expect(resp.Header.Get("Content-Security-Policy")).To(ContainSubstring("frame-ancestors 'none'"))
	etag := resp.Header.Get("ETag")
	g.Expect(etag).NotTo(BeEmpty())

	for _, tag := range []string{etag, "W/" + etag} {
		req, err := http.NewRequest(http.MethodGet, env.apiURL+"/api/v1/nodes", nil)
		g.Expect(err).NotTo(HaveOccurred())
		req.Header.Set("If-None-Match", tag)
		resp, err := env.http.Do(req)
		g.Expect(err).NotTo(HaveOccurred())
		_ = resp.Body.Close()
		g.Expect(resp.StatusCode).To(Equal(http.StatusNotModified), "If-None-Match: %s", tag)
	}

	g.Expect(apiGet("/api/v1/nodes/no-such-node/pods", nil)).To(Equal(http.StatusNotFound))
	g.Expect(apiGet("/no-such-path", nil)).To(Equal(http.StatusNotFound))

	resp, err = env.http.Get(env.apiURL + "/")
	g.Expect(err).NotTo(HaveOccurred())
	_ = resp.Body.Close()
	g.Expect(resp.StatusCode).To(Equal(http.StatusOK))
	g.Expect(resp.Header.Get("Content-Type")).To(HavePrefix("text/html"))
}

// The operator already runs as its ServiceAccount, which proves the granted
// permissions are enough. This checks they are not more than that. Entries read like
// `kubectl auth can-i`: "verb resource[.group][/subresource] [namespace]".
func TestRBACLeastPrivilege(t *testing.T) {
	g := NewWithT(t)
	allowed := []string{
		"list nodes", "watch nodes", "list pods", "watch pods",
		"get clusterviews.observe.samkeet.org",
		"list clusterviews.observe.samkeet.org",
		"watch clusterviews.observe.samkeet.org",
		"patch clusterviews.observe.samkeet.org/status",
		// Cached for the API's pod events drawer.
		"list events.events.k8s.io", "watch events.events.k8s.io",
		"get leases.coordination.k8s.io " + operatorNS,
		"create leases.coordination.k8s.io " + operatorNS,
		"update leases.coordination.k8s.io " + operatorNS,
		"create events " + operatorNS,
		"patch events " + operatorNS,
	}
	denied := []string{
		"get secrets " + operatorNS, "list secrets", "list configmaps",
		"get nodes", "patch nodes", "get nodes/proxy",
		"get pods default", "create pods default", "delete pods default",
		"get pods/log default", "create pods/exec default",
		"create clusterviews.observe.samkeet.org",
		"update clusterviews.observe.samkeet.org",
		"delete clusterviews.observe.samkeet.org",
		"update clusterviews.observe.samkeet.org/status",
		"delete leases.coordination.k8s.io " + operatorNS,
		"create leases.coordination.k8s.io default",
		"create events default",
		"get events.events.k8s.io default",
		"create events.events.k8s.io default",
		"delete events.events.k8s.io default",
	}
	for _, a := range allowed {
		g.Expect(canI(g, a)).To(BeTrue(), "operator should be allowed to %q", a)
	}
	for _, a := range denied {
		g.Expect(canI(g, a)).To(BeFalse(), "operator should be denied %q", a)
	}
}

// canI asks the API server whether the operator's ServiceAccount may do access.
func canI(g Gomega, access string) bool {
	fields := strings.Fields(access)
	g.Expect(len(fields)).To(BeNumerically(">=", 2), "malformed access %q", access)
	attrs := &authzv1.ResourceAttributes{Verb: fields[0]}
	if len(fields) > 2 {
		attrs.Namespace = fields[2]
	}
	res, sub, _ := strings.Cut(fields[1], "/")
	attrs.Resource, attrs.Group, _ = strings.Cut(res, ".")
	attrs.Subresource = sub
	sar := &authzv1.SubjectAccessReview{Spec: authzv1.SubjectAccessReviewSpec{
		User: serviceAccountUser(), Groups: serviceAccountGroups(), ResourceAttributes: attrs,
	}}
	g.Expect(env.admin.Create(context.Background(), sar)).To(Succeed())
	return sar.Status.Allowed
}

// watchStatusWrites records when the ClusterView is modified. Only the operator
// writes it during these tests, so every modification is one status write.
func watchStatusWrites(t *testing.T) func() []time.Time {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w, err := env.admin.Watch(ctx, &observev1alpha1.ClusterViewList{},
		client.MatchingFields{"metadata.name": viewName})
	if err != nil {
		cancel()
		t.Fatalf("watching ClusterView: %v", err)
	}
	writes := make(chan time.Time, 1024)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range w.ResultChan() {
			if ev.Type == watch.Modified {
				writes <- time.Now()
			}
		}
	}()
	stop := func() []time.Time {
		w.Stop()
		cancel()
		<-done
		close(writes)
		var times []time.Time
		for ts := range writes {
			times = append(times, ts)
		}
		slices.SortFunc(times, time.Time.Compare)
		return times
	}
	t.Cleanup(func() { cancel() })
	return stop
}

func gaps(times []time.Time) []time.Duration {
	var out []time.Duration
	for i := 1; i < len(times); i++ {
		out = append(out, times[i].Sub(times[i-1]).Round(time.Millisecond))
	}
	return out
}
