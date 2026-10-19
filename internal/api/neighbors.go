package api

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/neigh"
)

// NeighborList reads the kernel's neighbour table (internal/neigh). It
// reads only: it needs no network management and works wherever janusd
// runs, in its own network namespace.
func (n *Network) NeighborList(_ context.Context, req *janusv1alpha1.NeighborListRequest) (*janusv1alpha1.NeighborListResponse, error) {
	ns, err := neigh.List(req.GetInterface())
	if errors.Is(err, neigh.ErrNoInterface) {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &janusv1alpha1.NeighborListResponse{Neighbors: ns}, nil
}
