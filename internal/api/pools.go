package api

import (
	"context"
	"fmt"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/samkeet/clusterview-operator/internal/clusterstate"
)

// pools serves the node pool capacity map: pools, their nodes, and every
// non-terminated pod's effective requests and limits, in one ETag'd body. The
// namespace filter applies to pods only, as everywhere else in the API.
func (s *Server) pools(w http.ResponseWriter, r *http.Request) {
	snap, err := s.poolsSnapshot(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.writeJSON(w, r, snap)
}

func (s *Server) poolsSnapshot(ctx context.Context) (clusterstate.PoolsSnapshot, error) {
	namespaces, err := s.namespaces(ctx)
	if err != nil {
		return clusterstate.PoolsSnapshot{}, err
	}
	var nodes corev1.NodeList
	// BuildPools only reads, so skip deep-copying every cached object on each poll.
	if err := s.Reader.List(ctx, &nodes, client.UnsafeDisableDeepCopy); err != nil {
		return clusterstate.PoolsSnapshot{}, fmt.Errorf("listing nodes: %w", err)
	}
	pods, err := s.listPods(ctx, namespaces)
	if err != nil {
		return clusterstate.PoolsSnapshot{}, err
	}
	return clusterstate.BuildPools(nodes.Items, pods, namespaces), nil
}
