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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
	"github.com/samkeet/clusterview-operator/internal/clusterstate"
)

const (
	pathPools    = "/api/v1/pools"
	poolA        = "pool-a"
	poolNodeType = "s-4vcpu-8gb"
	genericError = "internal error"
)

// poolNode is a node in pool-a, sized like the DOKS nodes.
func poolNode(name string) *corev1.Node {
	n := readyNode(name)
	n.Labels = map[string]string{
		clusterstate.NodePoolLabel:     poolA,
		corev1.LabelInstanceTypeStable: poolNodeType,
	}
	n.Status.Allocatable = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("3890m"),
		corev1.ResourceMemory: resource.MustParse("6568224Ki"),
	}
	return n
}

// requesting gives a test pod one container with a CPU and memory request.
func requesting(p *corev1.Pod, cpu, mem string) *corev1.Pod {
	p.Spec.Containers = []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		},
	}}}
	p.Status.QOSClass = corev1.PodQOSBurstable
	return p
}

// poolObjects: pool-a with n1 and n2, an unlabelled n3, and pods in namespaces
// a and b, including a terminated and an unscheduled one.
func poolObjects() []client.Object {
	n3 := readyNode("n3")
	n3.Spec.Unschedulable = true
	return []client.Object{
		poolNode("n1"), poolNode("n2"), n3,
		requesting(testPod("a", "web-0", "n1", corev1.PodRunning, 0), "100m", "128Mi"),
		requesting(testPod("b", "db-0", "n1", corev1.PodRunning, 0), "500m", "1Gi"),
		requesting(testPod("b", "job", "n2", corev1.PodSucceeded, 0), "1", "1Gi"),
		requesting(testPod("a", "pending", "", corev1.PodPending, 0), "1", "1Gi"),
	}
}

func TestPools(t *testing.T) {
	rec := get(t, newHandler(t, nil, poolObjects()...), pathPools)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	raw := decode[map[string]json.RawMessage](t, rec)
	if len(raw) != 2 || raw["pools"] == nil || string(raw["filtered"]) != "false" {
		t.Fatalf("top level = %s, want {filtered: false, pools: [...]}", rec.Body)
	}

	got := decode[clusterstate.PoolsSnapshot](t, rec)
	if len(got.Pools) != 2 || got.Pools[0].Name != poolA || got.Pools[1].Name != clusterstate.UnpooledPool {
		t.Fatalf("pools = %+v, want pool-a then %s", got.Pools, clusterstate.UnpooledPool)
	}
	a := got.Pools[0]
	if a.NodeCount != 2 || a.InstanceType != poolNodeType {
		t.Errorf("pool-a = %d nodes, type %q", a.NodeCount, a.InstanceType)
	}
	n1 := a.Nodes[0]
	want := clusterstate.NodeCapacity{
		Name:        "n1",
		Status:      "Ready",
		Schedulable: true,
		Allocatable: clusterstate.Allocatable{CPUMilli: 3890, MemoryBytes: 6568224 << 10},
		Totals:      clusterstate.CapacityTotals{CPURequestMilli: 600, MemRequestBytes: (128 + 1024) << 20},
		Pods: []clusterstate.PodCapacity{
			{Namespace: "a", Name: "web-0", QOS: corev1.PodQOSBurstable, CPURequestMilli: 100, MemRequestBytes: 128 << 20},
			{Namespace: "b", Name: "db-0", QOS: corev1.PodQOSBurstable, CPURequestMilli: 500, MemRequestBytes: 1 << 30},
		},
	}
	if !reflect.DeepEqual(n1, want) {
		t.Errorf("n1 =\n%+v\nwant\n%+v", n1, want)
	}
	if n2 := a.Nodes[1]; len(n2.Pods) != 0 || n2.Totals != (clusterstate.CapacityTotals{}) {
		t.Errorf("n2 = %+v, want no pods (its only pod Succeeded)", n2)
	}
	if n3 := got.Pools[1].Nodes[0]; n3.Schedulable {
		t.Errorf("n3 should be cordoned")
	}
	// Each pod carries exactly the seven agreed fields.
	if !strings.Contains(rec.Body.String(), `{"namespace":"a","name":"web-0","qos":"Burstable","cpuRequestMilli":100,`+
		`"cpuLimitMilli":0,"memRequestBytes":134217728,"memLimitBytes":0}`) {
		t.Errorf("pod JSON shape changed: %s", rec.Body)
	}
}

func TestPoolsEmptyClusterIsArrayNotNull(t *testing.T) {
	rec := get(t, newHandler(t, nil), pathPools)
	raw := decode[map[string]json.RawMessage](t, rec)
	if string(raw["pools"]) != "[]" {
		t.Errorf("pools = %s, want []", raw["pools"])
	}
	rec = get(t, newHandler(t, nil, readyNode("idle")), pathPools)
	if !strings.Contains(rec.Body.String(), `"pods":[]`) {
		t.Errorf("a node with no pods must have pods: [], got %s", rec.Body)
	}
}

func TestPoolsNamespaceFilter(t *testing.T) {
	h := newHandler(t, nil, append(poolObjects(), clusterView("a"))...)
	got := decode[clusterstate.PoolsSnapshot](t, get(t, h, pathPools))
	if !got.Filtered {
		t.Error("filtered = false with a ClusterView namespace filter")
	}
	// Nodes aren't namespaced, so all three stay.
	if len(got.Pools) != 2 || got.Pools[0].NodeCount != 2 || got.Pools[1].NodeCount != 1 {
		t.Fatalf("pools = %+v, want every node", got.Pools)
	}
	n1 := got.Pools[0].Nodes[0]
	if len(n1.Pods) != 1 || n1.Pods[0].Namespace != "a" {
		t.Errorf("n1 pods = %+v, want only namespace a", n1.Pods)
	}
	if n1.Totals.CPURequestMilli != 100 {
		t.Errorf("n1 cpu request total = %d, want 100 (filtered namespaces only)", n1.Totals.CPURequestMilli)
	}
}

func TestPoolsETag(t *testing.T) {
	h := newHandler(t, nil, poolObjects()...)
	first := get(t, h, pathPools)
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	rec := get(t, h, pathPools, "If-None-Match", etag)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("revalidation = %d with %d bytes, want an empty 304", rec.Code, rec.Body.Len())
	}

	// A pod's request changing is a real change, so the tag must move.
	changed := poolObjects()
	requesting(changed[3].(*corev1.Pod), "200m", "128Mi")
	other := get(t, newHandler(t, nil, changed...), pathPools, "If-None-Match", etag)
	if other.Code != http.StatusOK || other.Header().Get("ETag") == etag {
		t.Errorf("changed requests: status %d, ETag %q; want 200 and a new tag", other.Code, other.Header().Get("ETag"))
	}
}

func TestPoolsRejectsNonGET(t *testing.T) {
	h := newHandler(t, nil, poolObjects()...)
	for _, m := range []string{http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(m, pathPools, nil))
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
			t.Errorf("%s: status = %d, Allow = %q; want 405 and GET", m, rec.Code, rec.Header().Get("Allow"))
		}
	}
}

// If the namespace filter can't be read, showing every namespace would expose
// pods the user excluded, so the request fails instead.
func TestPoolsClusterViewErrorFailsClosed(t *testing.T) {
	funcs := &interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*observev1alpha1.ClusterView); ok {
				return errors.New("cache unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}
	rec := get(t, newHandler(t, funcs, poolObjects()...), pathPools)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if e := decode[errorResponse](t, rec); e.Error != genericError {
		t.Errorf("error = %q, want the generic message", e.Error)
	}
}

// richPod uses every input the capacity math reads (init and sidecar
// containers, overhead, pod-level resources, status resources, qosClass) plus
// the bulk StripPod removes.
func richPod() *corev1.Pod {
	q := resource.MustParse
	always := corev1.ContainerRestartPolicyAlways
	res := func(cpu, mem string) corev1.ResourceRequirements {
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: q(cpu), corev1.ResourceMemory: q(mem)},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: q(cpu), corev1.ResourceMemory: q(mem)},
		}
	}
	bulk := func(c corev1.Container) corev1.Container {
		c.Env = []corev1.EnvVar{{Name: "K", Value: "v"}}
		c.VolumeMounts = []corev1.VolumeMount{{Name: "v", MountPath: "/v"}}
		return c
	}
	podLevel := res("2", "2Gi")
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "a", Name: "rich",
			Annotations:   map[string]string{"big": strings.Repeat("x", 100)},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
		Spec: corev1.PodSpec{
			NodeName: "n1",
			Volumes:  []corev1.Volume{{Name: "v"}},
			Overhead: corev1.ResourceList{corev1.ResourceCPU: q("250m"), corev1.ResourceMemory: q("120Mi")},
			Resources: &corev1.ResourceRequirements{
				Requests: podLevel.Requests,
			},
			InitContainers: []corev1.Container{
				bulk(corev1.Container{Name: "init", Resources: res("1", "1Gi")}),
				bulk(corev1.Container{Name: "proxy", RestartPolicy: &always, Resources: res("50m", "64Mi")}),
			},
			Containers: []corev1.Container{bulk(corev1.Container{Name: "main", Resources: res("300m", "256Mi")})},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			QOSClass: corev1.PodQOSBurstable,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:               "main",
				AllocatedResources: corev1.ResourceList{corev1.ResourceCPU: q("400m"), corev1.ResourceMemory: q("256Mi")},
				Resources:          &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: q("600m")}},
			}},
		},
	}
}

// TestTransformsKeepPoolInputs guards the capacity map against the cache
// transforms: stripping a pod or node must never change what BuildPools sees.
func TestTransformsKeepPoolInputs(t *testing.T) {
	want := clusterstate.BuildPools([]corev1.Node{*poolNode("n1")}, []corev1.Pod{*richPod()}, nil)

	n, _ := StripNode(poolNode("n1"))
	p, _ := StripPod(richPod())
	got := clusterstate.BuildPools([]corev1.Node{*n.(*corev1.Node)}, []corev1.Pod{*p.(*corev1.Pod)}, nil)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildPools changed after transform\n got: %+v\nwant: %+v", got, want)
	}
	// The rich pod must actually exercise the math, or the test proves nothing.
	if pod := want.Pools[0].Nodes[0].Pods[0]; pod.CPURequestMilli == 0 || pod.CPULimitMilli == 0 {
		t.Errorf("rich pod computed as %+v; expected non-zero requests and limits", pod)
	}
}
