package clusterstate

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
)

const (
	mi = int64(1) << 20
	gi = int64(1) << 30
)

// rl builds a ResourceList from cpu and memory quantity strings; "" leaves one out.
func rl(cpu, mem string) corev1.ResourceList {
	l := corev1.ResourceList{}
	if cpu != "" {
		l[corev1.ResourceCPU] = resource.MustParse(cpu)
	}
	if mem != "" {
		l[corev1.ResourceMemory] = resource.MustParse(mem)
	}
	return l
}

func ctr(name string, req, lim corev1.ResourceList) corev1.Container {
	return corev1.Container{Name: name, Resources: corev1.ResourceRequirements{Requests: req, Limits: lim}}
}

func sidecar(name string, req, lim corev1.ResourceList) corev1.Container {
	c := ctr(name, req, lim)
	c.RestartPolicy = new(corev1.ContainerRestartPolicyAlways)
	return c
}

// ciliumPod mirrors cilium-pg45c on the live DOKS cluster. kubectl describe node
// shows it as 310m (7%) / 1 (25%) CPU and 332Mi (5%) / 1Gi (15%) memory on a
// 3890m / 6568224Ki node. Its limits come from an init container: the app
// containers' limits add up to only 50m / 64Mi.
func ciliumPod() corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: nsSystem, Name: "cilium-pg45c"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				ctr("anchor-ip-fix", rl("100m", "100Mi"), nil),
				ctr("mount-cgroup", nil, nil),
				ctr("apply-sysctl-overwrites", nil, nil),
				ctr("mount-bpf-fs", nil, nil),
				ctr("clean-cilium-state", nil, nil),
				ctr("install-cni-binaries", rl("100m", "10Mi"), rl("1", "1Gi")),
			},
			Containers: []corev1.Container{
				ctr("nrr-status-patcher", rl("10m", "32Mi"), rl("50m", "64Mi")),
				ctr("cilium-agent", rl("300m", "300Mi"), nil),
			},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			QOSClass: corev1.PodQOSBurstable,
			// The kubelet reports these on 1.37; they match the spec, so they change nothing.
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "cilium-agent", AllocatedResources: rl("300m", "300Mi"),
					Resources: &corev1.ResourceRequirements{Requests: rl("300m", "300Mi")}},
				{Name: "nrr-status-patcher", AllocatedResources: rl("10m", "32Mi"),
					Resources: &corev1.ResourceRequirements{Requests: rl("10m", "32Mi"), Limits: rl("50m", "64Mi")}},
			},
		},
	}
}

func specPod(spec corev1.PodSpec, qos corev1.PodQOSClass) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: nsDefault, Name: "p"},
		Spec:       spec,
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, QOSClass: qos},
	}
}

// Live cases are copied from `kubectl describe node` on the DOKS cluster. The
// synthetic ones are worked out by hand from the helper's documented rules
// (sidecar KEP 753 for init containers, overhead added to set limits only,
// pod-level resources replacing container totals, max(spec, status) on resize).
func TestPodRequestsLimits(t *testing.T) {
	type want struct{ cpuReq, cpuLim, memReq, memLim int64 }
	tests := []struct {
		name string
		pod  corev1.Pod
		want want
		qos  corev1.PodQOSClass
	}{
		{
			name: "live: cilium, limit from an init container",
			pod:  ciliumPod(),
			want: want{310, 1000, 332 * mi, 1 * gi},
			qos:  corev1.PodQOSBurstable,
		},
		{
			name: "live: do-node-agent, memory limit only",
			pod: specPod(corev1.PodSpec{
				InitContainers: []corev1.Container{ctr("dynamic-config", nil, nil)},
				Containers:     []corev1.Container{ctr("do-node-agent", rl("102m", "80Mi"), rl("", "300Mi"))},
			}, corev1.PodQOSBurstable),
			want: want{102, 0, 80 * mi, 300 * mi},
			qos:  corev1.PodQOSBurstable,
		},
		{
			name: "BestEffort: no resources at all",
			pod:  specPod(corev1.PodSpec{Containers: []corev1.Container{ctr("c", nil, nil)}}, corev1.PodQOSBestEffort),
			want: want{},
			qos:  corev1.PodQOSBestEffort,
		},
		{
			name: "app containers are summed",
			pod: specPod(corev1.PodSpec{Containers: []corev1.Container{
				ctr("a", rl("100m", "100Mi"), rl("200m", "200Mi")),
				ctr("b", rl("250m", "50Mi"), rl("500m", "100Mi")),
			}}, corev1.PodQOSBurstable),
			want: want{350, 700, 150 * mi, 300 * mi},
		},
		{
			name: "a larger init container sets the floor",
			pod: specPod(corev1.PodSpec{
				InitContainers: []corev1.Container{ctr("init", rl("1", "1Gi"), rl("2", "2Gi"))},
				Containers:     []corev1.Container{ctr("app", rl("100m", "100Mi"), rl("200m", "200Mi"))},
			}, corev1.PodQOSBurstable),
			want: want{1000, 2000, 1 * gi, 2 * gi},
		},
		{
			// Requests: sidecar runs alongside the app (100m+50m), and the regular init
			// after it runs alongside the sidecar (200m+50m), so max(150m, 250m).
			// Memory: max(128Mi+64Mi, 100Mi+64Mi). Limits: the sidecar's add to the app's.
			name: "sidecar before a regular init container",
			pod: specPod(corev1.PodSpec{
				InitContainers: []corev1.Container{
					sidecar("proxy", rl("50m", "64Mi"), rl("100m", "128Mi")),
					ctr("migrate", rl("200m", "100Mi"), nil),
				},
				Containers: []corev1.Container{ctr("app", rl("100m", "128Mi"), rl("200m", "256Mi"))},
			}, corev1.PodQOSBurstable),
			want: want{250, 300, 192 * mi, 384 * mi},
		},
		{
			// The regular init runs before the sidecar starts, so it's counted alone.
			name: "sidecar after a regular init container",
			pod: specPod(corev1.PodSpec{
				InitContainers: []corev1.Container{
					ctr("migrate", rl("200m", "100Mi"), nil),
					sidecar("proxy", rl("50m", "64Mi"), rl("100m", "128Mi")),
				},
				Containers: []corev1.Container{ctr("app", rl("100m", "128Mi"), rl("200m", "256Mi"))},
			}, corev1.PodQOSBurstable),
			want: want{200, 300, 192 * mi, 384 * mi},
		},
		{
			// Overhead is added to every request, but only to limits that are set.
			name: "pod overhead",
			pod: specPod(corev1.PodSpec{
				Overhead:   rl("250m", "120Mi"),
				Containers: []corev1.Container{ctr("app", rl("100m", "100Mi"), rl("500m", ""))},
			}, corev1.PodQOSBurstable),
			want: want{350, 750, 220 * mi, 0},
		},
		{
			// Pod-level values replace the container totals for the resources they set.
			name: "pod-level resources",
			pod: specPod(corev1.PodSpec{
				Resources: &corev1.ResourceRequirements{
					Requests: rl("1", "1Gi"),
					Limits:   rl("2", ""),
				},
				Containers: []corev1.Container{ctr("app", rl("100m", "100Mi"), rl("200m", "200Mi"))},
			}, corev1.PodQOSBurstable),
			want: want{1000, 2000, 1 * gi, 200 * mi},
		},
		{
			// During an in-place resize, the larger of spec and status counts.
			name: "in-place resize in progress",
			pod: func() corev1.Pod {
				p := specPod(corev1.PodSpec{
					Containers: []corev1.Container{ctr("app", rl("200m", "100Mi"), rl("1", "200Mi"))},
				}, corev1.PodQOSBurstable)
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:               "app",
					AllocatedResources: rl("500m", "100Mi"),
					Resources:          &corev1.ResourceRequirements{Requests: rl("500m", "100Mi"), Limits: rl("2", "200Mi")},
				}}
				return p
			}(),
			want: want{500, 2000, 100 * mi, 200 * mi},
		},
		{
			name: "decimal memory units",
			pod: specPod(corev1.PodSpec{
				Containers: []corev1.Container{ctr("app", rl("", "500M"), rl("", "1G"))},
			}, corev1.PodQOSBurstable),
			want: want{0, 0, 500_000_000, 1_000_000_000},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PodRequestsLimits(&tt.pod)
			gotW := want{got.CPURequestMilli, got.CPULimitMilli, got.MemRequestBytes, got.MemLimitBytes}
			if gotW != tt.want {
				t.Errorf("got %+v, want %+v", gotW, tt.want)
			}
			if got.Namespace != tt.pod.Namespace || got.Name != tt.pod.Name {
				t.Errorf("identity = %s/%s, want %s/%s", got.Namespace, got.Name, tt.pod.Namespace, tt.pod.Name)
			}
			if tt.qos != "" && got.QOS != tt.qos {
				t.Errorf("qos = %q, want %q", got.QOS, tt.qos)
			}
		})
	}
}

func TestTerminated(t *testing.T) {
	for phase, want := range map[corev1.PodPhase]bool{
		corev1.PodSucceeded: true,
		corev1.PodFailed:    true,
		corev1.PodRunning:   false,
		corev1.PodPending:   false,
		corev1.PodUnknown:   false,
		"":                  false,
	} {
		p := corev1.Pod{Status: corev1.PodStatus{Phase: phase}}
		if got := Terminated(&p); got != want {
			t.Errorf("Terminated(%q) = %v, want %v", phase, got, want)
		}
	}
}

func poolNode(name, pool, instanceType string) corev1.Node {
	n := node(name, status(corev1.ConditionTrue))
	n.Labels = map[string]string{}
	if pool != "-" {
		n.Labels[NodePoolLabel] = pool
	}
	if instanceType != "" {
		n.Labels[corev1.LabelInstanceTypeStable] = instanceType
	}
	n.Status.Allocatable = rl("3890m", "6568224Ki")
	return n
}

func boundPod(ns, name, nodeName string, phase corev1.PodPhase, req, lim corev1.ResourceList) corev1.Pod {
	p := pod(ns, name, nodeName, phase)
	p.Spec.Containers = []corev1.Container{ctr("c", req, lim)}
	return p
}

// TestBuildPoolsMatchesDescribe rebuilds node pool-qhbs5hlue-3efzx4 from the live
// cluster. kubectl describe node prints "Allocated resources": cpu 612m (15%) /
// 1 (25%), memory 752Mi (11%) / 1664Mi (25%).
func TestBuildPoolsMatchesDescribe(t *testing.T) {
	n := poolNode("pool-qhbs5hlue-3efzx4", "pool-qhbs5hlue", "s-4vcpu-8gb")
	pods := make([]corev1.Pod, 0, 10)
	pods = append(pods,
		ciliumPod(),
		boundPod(nsSystem, "coredns-6b4b4b84d8-lkf46", "", corev1.PodRunning, rl("100m", "170Mi"), rl("", "170Mi")),
		boundPod(nsSystem, "coredns-6b4b4b84d8-wgmlq", "", corev1.PodRunning, rl("100m", "170Mi"), rl("", "170Mi")),
		boundPod(nsSystem, "do-node-agent-f8lcw", "", corev1.PodRunning, rl("102m", "80Mi"), rl("", "300Mi")),
	)
	for _, name := range []string{"csi-do-node-kq6zg", "doks-telemetry-config-reloader-s6p9z", "hubble-relay-6fd68c8df5-6qjp7",
		"hubble-ui-7857984d59-7jv45", "konnectivity-agent-8d54854f-pvxgc", "konnectivity-agent-8d54854f-x52f4"} {
		pods = append(pods, boundPod(nsSystem, name, "", corev1.PodRunning, nil, nil))
	}
	for i := range pods {
		pods[i].Spec.NodeName = n.Name
	}

	snap := BuildPools([]corev1.Node{n}, pods, nil)
	if len(snap.Pools) != 1 || len(snap.Pools[0].Nodes) != 1 {
		t.Fatalf("got %+v, want one pool with one node", snap)
	}
	got := snap.Pools[0].Nodes[0]
	want := CapacityTotals{CPURequestMilli: 612, CPULimitMilli: 1000, MemRequestBytes: 752 * mi, MemLimitBytes: 1664 * mi}
	if got.Totals != want {
		t.Errorf("totals = %+v, want %+v", got.Totals, want)
	}
	if got.Allocatable != (Allocatable{CPUMilli: 3890, MemoryBytes: 6568224 << 10}) {
		t.Errorf("allocatable = %+v", got.Allocatable)
	}
	if len(got.Pods) != 10 {
		t.Errorf("pods = %d, want 10", len(got.Pods))
	}
	// describe's truncated percentages, computed the way the UI does.
	pct := func(v, total int64) int64 { return int64(float64(v) / float64(total) * 100) }
	for _, c := range []struct{ got, want int64 }{
		{pct(got.Totals.CPURequestMilli, got.Allocatable.CPUMilli), 15},
		{pct(got.Totals.CPULimitMilli, got.Allocatable.CPUMilli), 25},
		{pct(got.Totals.MemRequestBytes, got.Allocatable.MemoryBytes), 11},
		{pct(got.Totals.MemLimitBytes, got.Allocatable.MemoryBytes), 25},
		{pct(310, got.Allocatable.CPUMilli), 7}, // cilium: 7.97% prints as 7%
	} {
		if c.got != c.want {
			t.Errorf("percent = %d, want %d", c.got, c.want)
		}
	}
	if snap.Filtered {
		t.Error("Filtered = true with no namespace filter")
	}
}

func TestBuildPoolsGrouping(t *testing.T) {
	cordoned := poolNode("b-2", "pool-b", "s-8vcpu-16gb")
	cordoned.Spec.Unschedulable = true
	cordoned.Status.Conditions[0].Status = corev1.ConditionFalse
	noAlloc := poolNode("solo", "-", "")
	noAlloc.Status.Allocatable = nil
	noAlloc.Status.Capacity = rl("2", "4Gi")
	nodes := []corev1.Node{
		cordoned,
		poolNode("a-1", "pool-a", "s-4vcpu-8gb"),
		poolNode("b-1", "pool-b", "s-8vcpu-16gb"),
		noAlloc,
		poolNode("empty-label", "", "s-4vcpu-8gb"),
		poolNode("a-2", "pool-a", "s-2vcpu-4gb"),
	}
	r := rl("100m", "100Mi")
	pods := []corev1.Pod{
		boundPod("shop", "web-1", "a-1", corev1.PodRunning, r, nil),
		boundPod("batch", "job-1", "a-1", corev1.PodPending, r, nil),
		boundPod("batch", "done", "a-1", corev1.PodSucceeded, r, nil),
		boundPod("batch", "crashed", "a-1", corev1.PodFailed, r, nil),
		boundPod("shop", "unscheduled", "", corev1.PodPending, r, nil),
		boundPod("shop", "orphan", "deleted-node", corev1.PodRunning, r, nil),
		boundPod("hidden", "secret", "a-1", corev1.PodRunning, r, nil),
		boundPod("shop", "web-0", "a-1", corev1.PodRunning, r, nil),
	}

	snap := BuildPools(nodes, pods, []string{"shop", "batch"})

	if !snap.Filtered {
		t.Error("Filtered = false with a namespace filter")
	}
	type poolWant struct {
		name, instanceType string
		nodes              []string
	}
	wantPools := []poolWant{
		{"pool-a", "", []string{"a-1", "a-2"}}, // mixed instance types: left out
		{"pool-b", "s-8vcpu-16gb", []string{"b-1", "b-2"}},
		{UnpooledPool, "", []string{"empty-label", "solo"}}, // last, despite sorting before letters
	}
	if len(snap.Pools) != len(wantPools) {
		t.Fatalf("pools = %+v, want %d", snap.Pools, len(wantPools))
	}
	for i, w := range wantPools {
		p := snap.Pools[i]
		var names []string
		for _, n := range p.Nodes {
			names = append(names, n.Name)
		}
		if p.Name != w.name || p.InstanceType != w.instanceType || int(p.NodeCount) != len(w.nodes) ||
			strings.Join(names, ",") != strings.Join(w.nodes, ",") {
			t.Errorf("pool %d = %s %q %d %v, want %+v", i, p.Name, p.InstanceType, p.NodeCount, names, w)
		}
	}

	a1 := snap.Pools[0].Nodes[0]
	podNames := make([]string, 0, len(a1.Pods))
	for _, p := range a1.Pods {
		podNames = append(podNames, p.Namespace+"/"+p.Name)
	}
	// Non-terminated, in the filter, sorted by namespace then name.
	if got, want := strings.Join(podNames, ","), "batch/job-1,shop/web-0,shop/web-1"; got != want {
		t.Errorf("a-1 pods = %s, want %s", got, want)
	}
	if a1.Totals != (CapacityTotals{CPURequestMilli: 300, MemRequestBytes: 300 * mi}) {
		t.Errorf("a-1 totals = %+v", a1.Totals)
	}

	b2 := snap.Pools[1].Nodes[1]
	if b2.Schedulable || b2.Status != observev1alpha1.NodeNotReady {
		t.Errorf("b-2 = schedulable %v status %s, want cordoned and NotReady", b2.Schedulable, b2.Status)
	}
	if b2.Pods == nil {
		t.Error("b-2 pods = nil, want an empty list")
	}

	solo := snap.Pools[2].Nodes[1]
	if solo.Allocatable != (Allocatable{CPUMilli: 2000, MemoryBytes: 4 * gi}) {
		t.Errorf("solo allocatable = %+v, want capacity as the fallback", solo.Allocatable)
	}
}

func TestBuildPoolsEmpty(t *testing.T) {
	snap := BuildPools(nil, nil, nil)
	if snap.Pools == nil || len(snap.Pools) != 0 {
		t.Errorf("pools = %#v, want an empty, non-nil list", snap.Pools)
	}
}
