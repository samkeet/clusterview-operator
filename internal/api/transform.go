package api

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
)

// StripPod is a cache transform that drops the bulky parts of a Pod before it's
// stored. On big clusters managedFields, annotations (e.g. last-applied-configuration)
// and container env/volume specs make up most of a cached Pod, and nothing here reads
// them. It keeps everything clusterstate and the controller read: name, namespace,
// creationTimestamp, labels, ownerReferences, spec.nodeName and all of status.
//
// It mutates in place (the informer owns the freshly decoded object) and is
// idempotent, since an object can pass through the transform more than once.
func StripPod(obj any) (any, error) {
	// meta.Accessor also covers metadata-only (PartialObjectMetadata) watches.
	if m, err := meta.Accessor(obj); err == nil {
		m.SetManagedFields(nil)
		m.SetAnnotations(nil)
	}
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return obj, nil
	}
	p.Spec.Volumes = nil
	for i := range p.Spec.InitContainers {
		stripContainer(&p.Spec.InitContainers[i])
	}
	for i := range p.Spec.Containers {
		stripContainer(&p.Spec.Containers[i])
	}
	for i := range p.Spec.EphemeralContainers {
		stripContainer((*corev1.Container)(&p.Spec.EphemeralContainers[i].EphemeralContainerCommon))
	}
	return obj, nil
}

func stripContainer(c *corev1.Container) {
	c.Env = nil
	c.EnvFrom = nil
	c.VolumeMounts = nil
	c.VolumeDevices = nil
}

// StripNode is a cache transform that drops managedFields and status.images.
// The image list is often the largest field on a Node (every image on the host,
// with sizes) and nothing reads it. Labels, conditions, capacity, allocatable and
// nodeInfo are kept because clusterstate reads them.
func StripNode(obj any) (any, error) {
	if m, err := meta.Accessor(obj); err == nil {
		m.SetManagedFields(nil)
	}
	if n, ok := obj.(*corev1.Node); ok {
		n.Status.Images = nil
	}
	return obj, nil
}
