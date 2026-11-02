package api

import (
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
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

// maxNoteBytes caps a cached event message, "…" included. The events.k8s.io API
// limits note to 1 KiB, but events written through the legacy core/v1 API may
// not be checked, and one huge message shouldn't cost cache memory forever.
const maxNoteBytes = 1024

// ellipsis marks a truncated message, so the UI shows it with no extra logic.
const ellipsis = "…"

// StripEvent is a cache transform for events.k8s.io Events. It drops fields the
// events endpoint never reads: managedFields, annotations, related,
// deprecatedSource, reportingInstance and regarding's apiVersion/resourceVersion.
// It keeps regarding kind/namespace/name/uid/fieldPath (index and UID filter),
// type, reason, note, the series, deprecated count and every timestamp.
//
// The note is cut to maxNoteBytes on a UTF-8 boundary and ends in "…". The cut
// leaves room for the ellipsis, so a second pass changes nothing.
func StripEvent(obj any) (any, error) {
	if m, err := meta.Accessor(obj); err == nil {
		m.SetManagedFields(nil)
		m.SetAnnotations(nil)
	}
	e, ok := obj.(*eventsv1.Event)
	if !ok {
		return obj, nil
	}
	e.Related = nil
	e.DeprecatedSource = corev1.EventSource{}
	e.ReportingInstance = ""
	e.Regarding.APIVersion = ""
	e.Regarding.ResourceVersion = ""
	e.Note = truncateNote(e.Note)
	return obj, nil
}

// truncateNote returns s unchanged if it fits in maxNoteBytes, and otherwise the
// longest prefix of whole runes that fits together with the ellipsis.
func truncateNote(s string) string {
	if len(s) <= maxNoteBytes {
		return s
	}
	cut := maxNoteBytes - len(ellipsis)
	// Step back to a rune start so a multi-byte character is never split.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
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
