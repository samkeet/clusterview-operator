//go:build integration

package integration

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
	"github.com/samkeet/clusterview-operator/internal/clusterstate"
)

const (
	// converge bounds how long the operator may take to reflect a change: one
	// throttle window plus slack for informer and watch latency.
	converge = 3 * refresh
	poll     = 250 * time.Millisecond

	// kwok never pulls images, so any name works; pause is the honest one.
	pauseImage    = "pause"
	appContainer  = "app"
	initContainer = "init"
)

// expected is the test oracle: Build over a direct, uncached list of Nodes and Pods
// with the ClusterView's current filter. The operator computes the same from its
// informer cache, so a difference is a caching, filtering, wiring or RBAC bug. Build's
// own logic is covered by its unit tests and by the explicit checks in each test.
func expected(ctx context.Context) (clusterstate.Snapshot, error) {
	cv, err := getView(ctx)
	if err != nil {
		return clusterstate.Snapshot{}, err
	}
	var nodes corev1.NodeList
	if err := env.admin.List(ctx, &nodes); err != nil {
		return clusterstate.Snapshot{}, fmt.Errorf("listing nodes: %w", err)
	}
	var pods corev1.PodList
	if err := env.admin.List(ctx, &pods); err != nil {
		return clusterstate.Snapshot{}, fmt.Errorf("listing pods: %w", err)
	}
	return roundTrip(clusterstate.Build(nodes.Items, pods.Items, cv.Spec.Namespaces))
}

// expectedPodsOn mirrors /api/v1/nodes/{name}/pods: filtered, converted, sorted.
func expectedPodsOn(ctx context.Context, node string) ([]clusterstate.PodInfo, error) {
	cv, err := getView(ctx)
	if err != nil {
		return nil, err
	}
	var pods corev1.PodList
	if err := env.admin.List(ctx, &pods); err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	infos := []clusterstate.PodInfo{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Spec.NodeName == node && clusterstate.InNamespaces(p.Namespace, cv.Spec.Namespaces) {
			infos = append(infos, clusterstate.NewPodInfo(p))
		}
	}
	slices.SortFunc(infos, func(a, b clusterstate.PodInfo) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return roundTrip(infos)
}

// roundTrip passes v through JSON so it compares equal to a decoded API response
// (time zones and precision included).
func roundTrip[T any](v T) (T, error) {
	var out T
	b, err := json.Marshal(v)
	if err != nil {
		return out, fmt.Errorf("encoding: %w", err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, fmt.Errorf("decoding: %w", err)
	}
	return out, nil
}

func getView(ctx context.Context) (*observev1alpha1.ClusterView, error) {
	var cv observev1alpha1.ClusterView
	if err := env.admin.Get(ctx, client.ObjectKey{Name: viewName}, &cv); err != nil {
		return nil, fmt.Errorf("getting ClusterView: %w", err)
	}
	return &cv, nil
}

// statusOf projects a Snapshot onto the fields the controller writes to the CR.
func statusOf(s clusterstate.Snapshot) observev1alpha1.ClusterViewStatus {
	st := observev1alpha1.ClusterViewStatus{
		NodeCount:     s.Summary.NodeCount,
		ReadyNodes:    s.Summary.ReadyNodes,
		NotReadyNodes: s.Summary.NotReadyNodes,
		PodCount:      s.Summary.PodCount,
	}
	if len(s.Summary.PodPhases) > 0 {
		st.PodPhases = s.Summary.PodPhases
	}
	for _, n := range s.Nodes {
		st.Nodes = append(st.Nodes, observev1alpha1.NodeSummary{Name: n.Name, Status: n.Status, PodCount: n.PodCount})
	}
	return st
}

// countsOf drops the bookkeeping fields so only the aggregated data is compared.
func countsOf(st observev1alpha1.ClusterViewStatus) observev1alpha1.ClusterViewStatus {
	return observev1alpha1.ClusterViewStatus{
		NodeCount: st.NodeCount, ReadyNodes: st.ReadyNodes, NotReadyNodes: st.NotReadyNodes,
		PodCount: st.PodCount, PodPhases: st.PodPhases, Nodes: st.Nodes,
	}
}

// expectConverged asserts that the CR status, /api/v1/nodes and /api/v1/summary all
// match the oracle. Run it inside Eventually.
func expectConverged(g Gomega) {
	ctx := context.Background()
	want, err := expected(ctx)
	g.Expect(err).NotTo(HaveOccurred())

	var got clusterstate.Snapshot
	g.Expect(apiGet("/api/v1/nodes", &got)).To(Equal(http.StatusOK))
	g.Expect(got).To(Equal(want), "/api/v1/nodes differs from the oracle")

	var sum clusterstate.Summary
	g.Expect(apiGet("/api/v1/summary", &sum)).To(Equal(http.StatusOK))
	g.Expect(sum).To(Equal(want.Summary), "/api/v1/summary differs from the oracle")

	cv, err := getView(ctx)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(countsOf(cv.Status)).To(Equal(statusOf(want)), "ClusterView status differs from the oracle")
	g.Expect(cv.Status.ObservedGeneration).To(Equal(cv.Generation))
	g.Expect(cv.Status.Conditions).To(ContainElement(And(
		HaveField("Type", "Ready"), HaveField("Status", metav1.ConditionTrue))))
}

// eventuallyQuiet waits until earlier tests' cleanup has finished (namespaces gone,
// no terminating pods) and the operator has caught up. Converged alone isn't enough
// for tests that count writes: a snapshot taken mid-teardown can match the oracle
// and still be followed by a legitimate write once the teardown completes.
func eventuallyQuiet(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	NewWithT(t).Eventually(func(g Gomega) {
		var nsList corev1.NamespaceList
		g.Expect(env.admin.List(ctx, &nsList)).To(Succeed())
		for _, ns := range nsList.Items {
			g.Expect(ns.Name).NotTo(HavePrefix(nsPrefix), "namespace still being deleted")
		}
		var pods corev1.PodList
		g.Expect(env.admin.List(ctx, &pods)).To(Succeed())
		for _, p := range pods.Items {
			g.Expect(p.DeletionTimestamp).To(BeNil(), "pod %s/%s still terminating", p.Namespace, p.Name)
		}
	}).WithTimeout(time.Minute).WithPolling(poll).Should(Succeed())
	eventuallyConverged(t)
}

func eventuallyConverged(t *testing.T) {
	t.Helper()
	NewWithT(t).Eventually(expectConverged).WithTimeout(converge).WithPolling(poll).Should(Succeed())
}

// apiGet decodes a JSON response into v (if non-nil) and returns the status code.
func apiGet(path string, v any) (int, error) {
	resp, err := env.http.Get(env.apiURL + path)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, fmt.Errorf("reading %s: %w", path, err)
	}
	if v != nil && resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, v); err != nil {
			return resp.StatusCode, fmt.Errorf("decoding %s: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

var nonDNS = regexp.MustCompile(`[^a-z0-9-]+`)

// dnsName turns a test name into a valid, unique-enough object name.
func dnsName(prefix string, t *testing.T) string {
	n := strings.Trim(nonDNS.ReplaceAllString(strings.ToLower(t.Name()), "-"), "-")
	return strings.TrimRight((prefix + n)[:min(len(prefix+n), 55)], "-")
}

// newNamespace creates a namespace for the test and deletes it (and its pods) after.
// It waits for the default ServiceAccount, which pod admission requires.
func newNamespace(t *testing.T, suffix string) string {
	t.Helper()
	ctx := context.Background()
	name := dnsName(nsPrefix, t) + suffix
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := env.admin.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace %s: %v", name, err)
	}
	t.Cleanup(func() {
		if err := deleteNamespace(context.Background(), env.admin, name); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	NewWithT(t).Eventually(func() error {
		return env.admin.Get(ctx, client.ObjectKey{Namespace: name, Name: "default"}, &corev1.ServiceAccount{})
	}).WithTimeout(30 * time.Second).WithPolling(poll).Should(Succeed())
	return name
}

// newManualNode creates a Ready node that kwok ignores, so its readiness from then on
// is whatever the test sets. The taint keeps the scheduler from placing other tests'
// pods on it.
func newManualNode(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	name := dnsName("manual-", t)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{manualLabel: ""}},
		Spec: corev1.NodeSpec{Taints: []corev1.Taint{{
			Key: manualLabel, Effect: corev1.TaintEffectNoSchedule,
		}}},
	}
	if err := env.admin.Create(ctx, node); err != nil {
		t.Fatalf("creating node %s: %v", name, err)
	}
	t.Cleanup(func() {
		if err := client.IgnoreNotFound(env.admin.Delete(context.Background(), node)); err != nil {
			t.Errorf("cleanup: deleting node %s: %v", name, err)
		}
	})
	res := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
	}
	// Merge patches throughout: kube-controller-manager edits new nodes (taints,
	// annotations) right away, so an Update would race it on resourceVersion.
	base := node.DeepCopy()
	node.Status.Capacity, node.Status.Allocatable = res, res
	if err := env.admin.Status().Patch(ctx, node, client.MergeFrom(base)); err != nil {
		t.Fatalf("setting node %s capacity: %v", name, err)
	}
	setNodeReady(t, name, corev1.ConditionTrue)
	return name
}

// setNodeReady sets the Ready condition; an empty status removes it (Unknown).
func setNodeReady(t *testing.T, name string, ready corev1.ConditionStatus) {
	t.Helper()
	ctx := context.Background()
	var node corev1.Node
	if err := env.admin.Get(ctx, client.ObjectKey{Name: name}, &node); err != nil {
		t.Fatalf("getting node %s: %v", name, err)
	}
	base := node.DeepCopy()
	node.Status.Conditions = nil
	if ready != "" {
		now := metav1.Now()
		node.Status.Conditions = []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: ready, Reason: "IntegrationTest",
			LastHeartbeatTime: now, LastTransitionTime: now,
		}}
	}
	if err := env.admin.Status().Patch(ctx, &node, client.MergeFrom(base)); err != nil {
		t.Fatalf("setting node %s Ready=%q: %v", name, ready, err)
	}
}

// bindPod creates a pod already bound to node. On a manual node it stays as created
// until the test sets its status, since no kubelet or kwok touches it.
func bindPod(t *testing.T, ns, name, node string) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PodSpec{
			NodeName:       node,
			InitContainers: []corev1.Container{{Name: initContainer, Image: pauseImage}},
			Containers:     []corev1.Container{{Name: appContainer, Image: pauseImage}},
			Tolerations:    []corev1.Toleration{{Key: manualLabel, Operator: corev1.TolerationOpExists}},
		},
	}
	if err := env.admin.Create(context.Background(), pod); err != nil {
		t.Fatalf("creating pod %s/%s: %v", ns, name, err)
	}
	return pod
}

// setPodStatus sets the phase and per-container restart counts.
func setPodStatus(t *testing.T, pod *corev1.Pod, phase corev1.PodPhase, initRestarts, appRestarts int32) {
	t.Helper()
	state := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	switch phase {
	case corev1.PodSucceeded:
		state = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
	case corev1.PodFailed:
		state = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}
	}
	done := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
	base := pod.DeepCopy()
	pod.Status.Phase = phase
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: initContainer, Image: pauseImage, RestartCount: initRestarts, State: done,
	}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: appContainer, Image: pauseImage, RestartCount: appRestarts, State: state,
	}}
	if err := env.admin.Status().Patch(context.Background(), pod, client.MergeFrom(base)); err != nil {
		t.Fatalf("setting status of pod %s/%s: %v", pod.Namespace, pod.Name, err)
	}
}

func setFilter(t *testing.T, namespaces []string) {
	t.Helper()
	ctx := context.Background()
	cv, err := getView(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cv.Spec.Namespaces = namespaces
	if err := env.admin.Update(ctx, cv); err != nil {
		t.Fatalf("setting namespace filter %v: %v", namespaces, err)
	}
}

// logUsage records the operator's current RSS, so a test's memory cost shows in -v
// output. Peak RSS and total CPU are printed once the operator exits.
func logUsage(t *testing.T) {
	t.Helper()
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(env.operator.Process.Pid)).Output()
	if err != nil {
		t.Logf("operator usage: ps failed: %v", err)
		return
	}
	kib, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Logf("operator usage: parsing %q: %v", out, err)
		return
	}
	t.Logf("operator RSS after %s: %.1f MiB", t.Name(), kib/1024)
}
