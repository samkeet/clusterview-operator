package api

import (
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/samkeet/clusterview-operator/internal/clusterstate"
)

// Shared test strings (goconst).
const (
	caseStripped    = "already stripped"
	casePassThrough = "non-object passes through"
	managerKubelet  = "kubelet"
	nodeName1       = "node-1"
	tombstone       = "tombstone"
)

func fullContainer(name string) corev1.Container {
	return corev1.Container{
		Name:          name,
		Image:         "img:" + name,
		Env:           []corev1.EnvVar{{Name: "K", Value: "V"}},
		EnvFrom:       []corev1.EnvFromSource{{Prefix: "P"}},
		VolumeMounts:  []corev1.VolumeMount{{Name: "v", MountPath: "/v"}},
		VolumeDevices: []corev1.VolumeDevice{{Name: "d", DevicePath: "/dev/d"}},
	}
}

// fullPod has every field the transform strips and every field it must keep.
func fullPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "ns",
			Name:              "p",
			UID:               "uid-1",
			ResourceVersion:   "42",
			CreationTimestamp: metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)),
			Labels:            map[string]string{"app": "x"},
			Annotations:       map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{...}"},
			OwnerReferences:   []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "rs"}},
			ManagedFields:     []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
		},
		Spec: corev1.PodSpec{
			NodeName:       "n1",
			Volumes:        []corev1.Volume{{Name: "v"}},
			InitContainers: []corev1.Container{fullContainer("init")},
			Containers:     []corev1.Container{fullContainer("app")},
			EphemeralContainers: []corev1.EphemeralContainer{{
				EphemeralContainerCommon: corev1.EphemeralContainerCommon(fullContainer("debug")),
			}},
		},
		Status: corev1.PodStatus{
			Phase:                 corev1.PodRunning,
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "init", RestartCount: 1}},
			ContainerStatuses:     []corev1.ContainerStatus{{Name: "app", RestartCount: 2}},
		},
	}
}

func fullNode() *corev1.Node {
	rl := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "n1",
			Labels:        map[string]string{corev1.LabelInstanceTypeStable: "s-4vcpu-8gb"},
			Annotations:   map[string]string{"a": "b"},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: managerKubelet}},
		},
		Spec: corev1.NodeSpec{Unschedulable: true},
		Status: corev1.NodeStatus{
			Capacity:    rl,
			Allocatable: rl,
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			NodeInfo:    corev1.NodeSystemInfo{Architecture: "amd64", KubeletVersion: "v1.34.0"},
			Images:      []corev1.ContainerImage{{Names: []string{"img"}, SizeBytes: 1 << 20}},
		},
	}
}

func TestStripPod(t *testing.T) {
	strippedPod := func() *corev1.Pod {
		p := fullPod()
		p.ManagedFields = nil
		p.Annotations = nil
		p.Spec.Volumes = nil
		for _, c := range []*corev1.Container{
			&p.Spec.InitContainers[0], &p.Spec.Containers[0],
			(*corev1.Container)(&p.Spec.EphemeralContainers[0].EphemeralContainerCommon),
		} {
			c.Env, c.EnvFrom, c.VolumeMounts, c.VolumeDevices = nil, nil, nil, nil
		}
		return p
	}
	tests := []struct {
		name string
		in   any
		want any
	}{
		{name: "full pod", in: fullPod(), want: strippedPod()},
		// A second pass must change nothing: informers can re-run the transform.
		{name: caseStripped, in: strippedPod(), want: strippedPod()},
		{
			name: "metadata-only pod",
			in: &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
				Name: "p", Annotations: map[string]string{"a": "b"},
				ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "m"}},
			}},
			want: &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "p"}},
		},
		{name: casePassThrough, in: tombstone, want: tombstone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := StripPod(tt.in)
			if err != nil {
				t.Fatalf("StripPod: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("StripPod mismatch\n got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

func TestStripNode(t *testing.T) {
	strippedNode := func() *corev1.Node {
		n := fullNode()
		n.ManagedFields = nil
		n.Status.Images = nil
		return n
	}
	tests := []struct {
		name string
		in   any
		want any
	}{
		{name: "full node", in: fullNode(), want: strippedNode()},
		{name: caseStripped, in: strippedNode(), want: strippedNode()},
		{name: casePassThrough, in: 7, want: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := StripNode(tt.in)
			if err != nil {
				t.Fatalf("StripNode: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("StripNode mismatch\n got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

// TestTransformsKeepClusterstateInputs guards the contract that matters most: the
// transforms must never remove anything clusterstate reads, or the UI and the CR
// status would silently change.
func TestTransformsKeepClusterstateInputs(t *testing.T) {
	want := clusterstate.Build([]corev1.Node{*fullNode()}, []corev1.Pod{*fullPod()}, nil)

	n, _ := StripNode(fullNode())
	p, _ := StripPod(fullPod())
	got := clusterstate.Build([]corev1.Node{*n.(*corev1.Node)}, []corev1.Pod{*p.(*corev1.Pod)}, nil)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Build changed after transform\n got: %+v\nwant: %+v", got, want)
	}
	if pi, wi := clusterstate.NewPodInfo(p.(*corev1.Pod)), clusterstate.NewPodInfo(fullPod()); pi != wi {
		t.Errorf("NewPodInfo changed after transform: got %+v, want %+v", pi, wi)
	}
}

func TestPodNodeName(t *testing.T) {
	tests := []struct {
		name     string
		nodeName string
		want     []string
	}{
		{name: "scheduled", nodeName: "n1", want: []string{"n1"}},
		{name: "unscheduled is not indexed", nodeName: "", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PodNodeName(&corev1.Pod{Spec: corev1.PodSpec{NodeName: tt.nodeName}})
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("PodNodeName = %v, want %v", got, tt.want)
			}
		})
	}
	if got := PodNodeName(&corev1.Node{}); got != nil {
		t.Errorf("PodNodeName(non-pod) = %v, want nil", got)
	}
}

// fullEvent has every field StripEvent drops and every field it must keep.
func fullEvent() *eventsv1.Event {
	created := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	return &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "ns",
			Name:              "p.18e4",
			CreationTimestamp: created,
			Annotations:       map[string]string{"a": "b"},
			ManagedFields:     []metav1.ManagedFieldsEntry{{Manager: managerKubelet}},
		},
		EventTime:           metav1.NewMicroTime(created.Time),
		Series:              &eventsv1.EventSeries{Count: 3, LastObservedTime: metav1.NewMicroTime(created.Add(time.Minute))},
		ReportingController: managerKubelet,
		ReportingInstance:   nodeName1,
		Action:              "Pulling",
		Reason:              "Pulling",
		Note:                "Pulling image",
		Type:                corev1.EventTypeNormal,
		Regarding: corev1.ObjectReference{
			APIVersion: "v1", Kind: kindPod, Namespace: "ns", Name: "p", UID: "uid-1",
			ResourceVersion: "42", FieldPath: "spec.containers{app}",
		},
		Related:                  &corev1.ObjectReference{Kind: "Node", Name: nodeName1},
		DeprecatedSource:         corev1.EventSource{Component: managerKubelet, Host: nodeName1},
		DeprecatedFirstTimestamp: created,
		DeprecatedLastTimestamp:  created,
		DeprecatedCount:          3,
	}
}

func strippedEvent() *eventsv1.Event {
	e := fullEvent()
	e.ManagedFields = nil
	e.Annotations = nil
	e.Related = nil
	e.DeprecatedSource = corev1.EventSource{}
	e.ReportingInstance = ""
	e.Regarding.APIVersion = ""
	e.Regarding.ResourceVersion = ""
	return e
}

func TestStripEvent(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want any
	}{
		{name: "full event", in: fullEvent(), want: strippedEvent()},
		{name: caseStripped, in: strippedEvent(), want: strippedEvent()},
		{name: casePassThrough, in: tombstone, want: tombstone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := StripEvent(tt.in)
			if err != nil {
				t.Fatalf("StripEvent: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("StripEvent mismatch\n got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}

func TestTruncateNote(t *testing.T) {
	// "é" is 2 bytes, so a cut at an odd offset would split it without the rune check.
	long2 := strings.Repeat("é", maxNoteBytes)
	tests := []struct {
		name      string
		in        string
		want      string
		truncated bool
	}{
		{name: "empty", in: "", want: ""},
		{name: "short", in: "Container started", want: "Container started"},
		{name: "exactly the cap", in: strings.Repeat("a", maxNoteBytes), want: strings.Repeat("a", maxNoteBytes)},
		{name: "one over the cap", in: strings.Repeat("a", maxNoteBytes+1),
			want: strings.Repeat("a", maxNoteBytes-len(ellipsis)) + ellipsis, truncated: true},
		{name: "multi-byte runes", in: long2,
			want: strings.Repeat("é", (maxNoteBytes-len(ellipsis))/2) + ellipsis, truncated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateNote(tt.in)
			if got != tt.want {
				t.Fatalf("truncateNote: got %d bytes %q..., want %d bytes", len(got), got[:min(len(got), 20)], len(tt.want))
			}
			if len(got) > maxNoteBytes || !utf8.ValidString(got) {
				t.Errorf("result is %d bytes (valid UTF-8: %v), want <= %d and valid", len(got), utf8.ValidString(got), maxNoteBytes)
			}
			if strings.HasSuffix(got, ellipsis) != tt.truncated {
				t.Errorf("ellipsis present = %v, want %v", !tt.truncated, tt.truncated)
			}
			// Informers can re-run the transform: a second pass must be a no-op.
			if again := truncateNote(got); again != got {
				t.Errorf("second pass changed the note")
			}
		})
	}

	// And through the transform itself.
	e := fullEvent()
	e.Note = strings.Repeat("x", 5000)
	out, _ := StripEvent(e)
	if n := out.(*eventsv1.Event).Note; len(n) != maxNoteBytes || !strings.HasSuffix(n, ellipsis) {
		t.Errorf("StripEvent note = %d bytes, want %d ending in %q", len(n), maxNoteBytes, ellipsis)
	}
}

func TestEventRegarding(t *testing.T) {
	tests := []struct {
		name string
		ref  corev1.ObjectReference
		want []string
	}{
		{name: "pod", ref: corev1.ObjectReference{Kind: kindPod, Namespace: "ns", Name: "p"}, want: []string{"Pod/ns/p"}},
		{name: "cluster-scoped", ref: corev1.ObjectReference{Kind: "Node", Name: "n1"}, want: []string{"Node//n1"}},
		{name: "no name is not indexed", ref: corev1.ObjectReference{Kind: kindPod, Namespace: "ns"}, want: nil},
		{name: "no kind is not indexed", ref: corev1.ObjectReference{Namespace: "ns", Name: "p"}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EventRegarding(&eventsv1.Event{Regarding: tt.ref})
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("EventRegarding = %v, want %v", got, tt.want)
			}
		})
	}
	if got := EventRegarding(&corev1.Pod{}); got != nil {
		t.Errorf("EventRegarding(non-event) = %v, want nil", got)
	}
	if got, want := RegardingKey(kindPod, "ns", "p"), "Pod/ns/p"; got != want {
		t.Errorf("RegardingKey = %q, want %q", got, want)
	}
}
