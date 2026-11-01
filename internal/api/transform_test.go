package api

import (
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/samkeet/clusterview-operator/internal/clusterstate"
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
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubelet"}},
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
		{name: "already stripped", in: strippedPod(), want: strippedPod()},
		{
			name: "metadata-only pod",
			in: &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
				Name: "p", Annotations: map[string]string{"a": "b"},
				ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "m"}},
			}},
			want: &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: "p"}},
		},
		{name: "non-object passes through", in: "tombstone", want: "tombstone"},
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
		{name: "already stripped", in: strippedNode(), want: strippedNode()},
		{name: "non-object passes through", in: 7, want: 7},
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
