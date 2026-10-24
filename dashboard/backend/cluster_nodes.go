package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc"

	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/clusters"
)

// A cluster's members are the Controller's nodes (Member.NodeID), reached
// over nodeproxy's connections.

// clusterNodes is how the engine reaches members: the tests replace it.
var clusterNodes = func(a *app) clusters.Nodes {
	return clusters.GRPC{
		Conn: func(_ context.Context, m clusters.Member) (grpc.ClientConnInterface, error) {
			n, err := a.memberNode(m)
			if err != nil {
				return nil, err
			}
			return nodeproxy.Conn(n)
		},
		ConfirmFirewall: func(_ context.Context, m clusters.Member, revertAt time.Time) error {
			n, err := a.memberNode(m)
			if err != nil {
				return err
			}
			if ok, msg := nodeproxy.ConfirmFirewall(n, revertAt); !ok {
				return errors.New(msg)
			}
			return nil
		},
	}
}

// memberNode is member m's node.
func (a *app) memberNode(m clusters.Member) (*store.Node, error) {
	n, ok := a.store.Get(m.NodeID)
	if !ok {
		return nil, fmt.Errorf("member %s: node %s is no longer one of the Controller's", m.Name, m.NodeID)
	}
	return n, nil
}

// clusterReady: m's node may join a cluster - one of the Controller's,
// trusting its fleet: what lets the Controller's calls through its lock.
func (a *app) clusterReady(_ context.Context, m clusters.Member) error {
	n, err := a.memberNode(m)
	if err != nil {
		return err
	}
	if !n.TrustsFleet() {
		return fmt.Errorf("%s doesn't trust the Controller's fleet yet: a cluster's member is managed through it (Fleet, then wait for the node to take the bundle)", n.Name)
	}
	return nil
}
