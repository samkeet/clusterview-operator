package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	observev1alpha1 "github.com/samkeet/clusterview-operator/api/v1alpha1"
	"github.com/samkeet/clusterview-operator/internal/clusterstate"
)

// nodePodsResponse wraps the list so fields can be added without breaking clients.
type nodePodsResponse struct {
	Node string                 `json:"node"`
	Pods []clusterstate.PodInfo `json:"pods"`
}

// errNodeNotFound lets nodePods tell a 404 apart from a cache failure.
var errNodeNotFound = errors.New("node not found")

func (s *Server) summary(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.writeJSON(w, r, snap.Summary)
}

// nodes returns the whole snapshot so the UI's header and cards always come from
// the same Build. Pod lists are never included; they load per node on demand.
func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	snap, err := s.snapshot(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.writeJSON(w, r, snap)
}

func (s *Server) nodePods(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	pods, err := s.podsOnNode(r.Context(), name)
	if errors.Is(err, errNodeNotFound) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", name))
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.writeJSON(w, r, nodePodsResponse{Node: name, Pods: pods})
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) snapshot(ctx context.Context) (clusterstate.Snapshot, error) {
	namespaces, err := s.namespaces(ctx)
	if err != nil {
		return clusterstate.Snapshot{}, err
	}
	var nodes corev1.NodeList
	// Build only reads, so skip deep-copying every cached object on each poll.
	if err := s.Reader.List(ctx, &nodes, client.UnsafeDisableDeepCopy); err != nil {
		return clusterstate.Snapshot{}, fmt.Errorf("listing nodes: %w", err)
	}
	pods, err := s.listPods(ctx, namespaces)
	if err != nil {
		return clusterstate.Snapshot{}, err
	}
	return clusterstate.Build(nodes.Items, pods, namespaces), nil
}

func (s *Server) podsOnNode(ctx context.Context, name string) ([]clusterstate.PodInfo, error) {
	var node corev1.Node
	if err := s.Reader.Get(ctx, client.ObjectKey{Name: name}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errNodeNotFound
		}
		return nil, fmt.Errorf("getting node %q: %w", name, err)
	}
	namespaces, err := s.namespaces(ctx)
	if err != nil {
		return nil, err
	}
	pods, err := s.listPods(ctx, namespaces, client.MatchingFields{PodNodeNameField: name})
	if err != nil {
		return nil, err
	}
	infos := make([]clusterstate.PodInfo, 0, len(pods))
	for i := range pods {
		infos = append(infos, clusterstate.NewPodInfo(&pods[i]))
	}
	// Stable order keeps the ETag stable across polls.
	slices.SortFunc(infos, func(a, b clusterstate.PodInfo) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return infos, nil
}

// namespaces reads the filter from the ClusterView on every request. It's a cache
// read, so it's cheap and always current. A missing ClusterView means "all
// namespaces"; any other error fails the request rather than falling back to all
// namespaces, which would show pods the user chose to exclude.
func (s *Server) namespaces(ctx context.Context) ([]string, error) {
	var cv observev1alpha1.ClusterView
	err := s.Reader.Get(ctx, client.ObjectKey{Name: s.ClusterViewName}, &cv)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting ClusterView %q: %w", s.ClusterViewName, err)
	}
	return cv.Spec.Namespaces, nil
}

// listPods lists one namespace at a time when a filter is set, so the cache's
// namespace index does the filtering and cost scales with the filtered pods only.
func (s *Server) listPods(ctx context.Context, namespaces []string, opts ...client.ListOption) ([]corev1.Pod, error) {
	opts = append(opts, client.UnsafeDisableDeepCopy)
	if len(namespaces) == 0 {
		var list corev1.PodList
		if err := s.Reader.List(ctx, &list, opts...); err != nil {
			return nil, fmt.Errorf("listing pods: %w", err)
		}
		return list.Items, nil
	}
	var pods []corev1.Pod
	for _, ns := range namespaces {
		var list corev1.PodList
		if err := s.Reader.List(ctx, &list, append(opts, client.InNamespace(ns))...); err != nil {
			return nil, fmt.Errorf("listing pods in namespace %q: %w", ns, err)
		}
		pods = append(pods, list.Items...)
	}
	return pods, nil
}
