package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/swenske/Janus/dashboard/backend/internal/nodeproxy"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// clusterNodeAPI is what the cluster engine asks of a member
// (docs/cluster.md sections 2-4): a seam the tests fake (clusterNodes).
// The context says for which cluster the engine acts
// (nodeproxy.ForCluster).
type clusterNodeAPI interface {
	Version(ctx context.Context, n *store.Node) (*janusv1alpha1.VersionResponse, error)
	Healthy(ctx context.Context, n *store.Node) error
	// Probe: n reaches target (host:port) over TCP.
	Probe(ctx context.Context, n *store.Node, target string) error
	CertificateRequest(ctx context.Context, n *store.Node) ([]byte, error)
	ClusterGet(ctx context.Context, n *store.Node) (*janusv1alpha1.ClusterInfo, error)
	ClusterSet(ctx context.Context, n *store.Node, req *janusv1alpha1.ClusterSetRequest) (*janusv1alpha1.ClusterInfo, error)
	ClusterLeave(ctx context.Context, n *store.Node) error
	ClusterStatus(ctx context.Context, n *store.Node) (*janusv1alpha1.ClusterStatusResponse, error)
	ClusterDrain(ctx context.Context, n *store.Node, timeout time.Duration) (string, error)
	ClusterUndrain(ctx context.Context, n *store.Node) error
	// ClusterPull: what came, one line each.
	ClusterPull(ctx context.Context, n *store.Node, from string, items []janusv1alpha1.ClusterItem) ([]string, error)
	RotateTickets(ctx context.Context, n *store.Node) error
	HAProxyConfig(ctx context.Context, n *store.Node) ([]byte, error)
	HAProxyValidate(ctx context.Context, n *store.Node, cfg []byte) error
	HAProxyApply(ctx context.Context, n *store.Node, cfg []byte) error
	Files(ctx context.Context, n *store.Node) ([]*janusv1alpha1.HAProxyFile, error)
	FileGet(ctx context.Context, n *store.Node, name string) ([]byte, error)
	FilePut(ctx context.Context, n *store.Node, name string, data []byte) error
	FileDelete(ctx context.Context, n *store.Node, name string) error
	CertificateUpload(ctx context.Context, n *store.Node, req *janusv1alpha1.CertificateUploadRequest) error
	// ModuleConfig and ModuleApply: vrrp, bgp, firewall - the firewall's
	// on trial, confirmed.
	ModuleConfig(ctx context.Context, n *store.Node, domain string) ([]byte, error)
	ModuleApply(ctx context.Context, n *store.Node, domain string, cfg []byte, validateOnly bool) error
}

var clusterNodes clusterNodeAPI = realClusterNodes{}

type realClusterNodes struct{}

// nodeError is a node's answer as the operator reads it.
func nodeError(err error) error {
	if err == nil {
		return nil
	}
	if s, ok := status.FromError(err); ok {
		return errors.New(s.Message())
	}
	return err
}

func (realClusterNodes) system(n *store.Node) (janusv1alpha1.SystemServiceClient, error) {
	conn, err := nodeproxy.Conn(n)
	if err != nil {
		return nil, err
	}
	return janusv1alpha1.NewSystemServiceClient(conn), nil
}

func (realClusterNodes) haproxy(n *store.Node) (janusv1alpha1.HAProxyServiceClient, error) {
	conn, err := nodeproxy.Conn(n)
	if err != nil {
		return nil, err
	}
	return janusv1alpha1.NewHAProxyServiceClient(conn), nil
}

func (realClusterNodes) network(n *store.Node) (janusv1alpha1.NetworkServiceClient, error) {
	conn, err := nodeproxy.Conn(n)
	if err != nil {
		return nil, err
	}
	return janusv1alpha1.NewNetworkServiceClient(conn), nil
}

func (realClusterNodes) cluster(n *store.Node) (janusv1alpha1.ClusterServiceClient, error) {
	conn, err := nodeproxy.Conn(n)
	if err != nil {
		return nil, err
	}
	return janusv1alpha1.NewClusterServiceClient(conn), nil
}

func (r realClusterNodes) Version(ctx context.Context, n *store.Node) (*janusv1alpha1.VersionResponse, error) {
	c, err := r.system(n)
	if err != nil {
		return nil, err
	}
	v, err := c.Version(ctx, &emptypb.Empty{})
	return v, nodeError(err)
}

func (r realClusterNodes) Healthy(ctx context.Context, n *store.Node) error {
	c, err := r.haproxy(n)
	if err != nil {
		return err
	}
	_, err = c.ShowInfo(ctx, &emptypb.Empty{})
	return nodeError(err)
}

func (r realClusterNodes) Probe(ctx context.Context, n *store.Node, target string) error {
	c, err := r.system(n)
	if err != nil {
		return err
	}
	stream, err := c.Probe(ctx, &janusv1alpha1.ProbeRequest{Kind: "tcp", Target: target})
	if err != nil {
		return nodeError(err)
	}
	var last string
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nodeError(err)
		}
		if ev.GetStage() == "connect" {
			if ev.GetOk() {
				return nil
			}
			last = ev.GetMessage()
		}
	}
	if last == "" {
		last = "no connection"
	}
	return errors.New(last)
}

func (r realClusterNodes) CertificateRequest(ctx context.Context, n *store.Node) ([]byte, error) {
	c, err := r.system(n)
	if err != nil {
		return nil, err
	}
	resp, err := c.CertificateRequest(ctx, &janusv1alpha1.CertificateRequestRequest{Purpose: janusv1alpha1.CertificatePurpose_CERTIFICATE_PURPOSE_CLUSTER_MEMBER})
	if err != nil {
		return nil, nodeError(err)
	}
	return resp.GetCsr(), nil
}

func (r realClusterNodes) ClusterGet(ctx context.Context, n *store.Node) (*janusv1alpha1.ClusterInfo, error) {
	c, err := r.cluster(n)
	if err != nil {
		return nil, err
	}
	info, err := c.ClusterGet(ctx, &emptypb.Empty{})
	return info, nodeError(err)
}

func (r realClusterNodes) ClusterSet(ctx context.Context, n *store.Node, req *janusv1alpha1.ClusterSetRequest) (*janusv1alpha1.ClusterInfo, error) {
	c, err := r.cluster(n)
	if err != nil {
		return nil, err
	}
	info, err := c.ClusterSet(ctx, req)
	return info, nodeError(err)
}

func (r realClusterNodes) ClusterLeave(ctx context.Context, n *store.Node) error {
	c, err := r.cluster(n)
	if err != nil {
		return err
	}
	_, err = c.ClusterLeave(ctx, &emptypb.Empty{})
	return nodeError(err)
}

func (r realClusterNodes) ClusterStatus(ctx context.Context, n *store.Node) (*janusv1alpha1.ClusterStatusResponse, error) {
	c, err := r.cluster(n)
	if err != nil {
		return nil, err
	}
	st, err := c.ClusterStatus(ctx, &emptypb.Empty{})
	return st, nodeError(err)
}

func (r realClusterNodes) ClusterDrain(ctx context.Context, n *store.Node, timeout time.Duration) (string, error) {
	c, err := r.cluster(n)
	if err != nil {
		return "", err
	}
	stream, err := c.ClusterDrain(ctx, &janusv1alpha1.ClusterDrainRequest{TimeoutSeconds: uint32(timeout.Seconds())})
	if err != nil {
		return "", nodeError(err)
	}
	var last *janusv1alpha1.ClusterDrainProgress
	for {
		p, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", nodeError(err)
		}
		last = p
	}
	if last == nil {
		return "", errors.New("the node said nothing of the drain")
	}
	if !last.GetDone() {
		return last.GetMessage(), errors.New(last.GetMessage())
	}
	return last.GetMessage(), nil
}

func (r realClusterNodes) ClusterUndrain(ctx context.Context, n *store.Node) error {
	c, err := r.cluster(n)
	if err != nil {
		return err
	}
	_, err = c.ClusterUndrain(ctx, &emptypb.Empty{})
	return nodeError(err)
}

func (r realClusterNodes) ClusterPull(ctx context.Context, n *store.Node, from string, items []janusv1alpha1.ClusterItem) ([]string, error) {
	c, err := r.cluster(n)
	if err != nil {
		return nil, err
	}
	stream, err := c.ClusterPull(ctx, &janusv1alpha1.ClusterPullRequest{From: from, Items: items})
	if err != nil {
		return nil, nodeError(err)
	}
	var lines []string
	for {
		p, err := stream.Recv()
		if err == io.EOF {
			return lines, nil
		}
		if err != nil {
			return lines, nodeError(err)
		}
		line := p.GetMessage()
		if p.GetName() != "" {
			line = p.GetName() + ": " + line
		}
		lines = append(lines, line)
	}
}

func (r realClusterNodes) RotateTickets(ctx context.Context, n *store.Node) error {
	c, err := r.cluster(n)
	if err != nil {
		return err
	}
	_, err = c.TLSTicketKeysRotate(ctx, &emptypb.Empty{})
	return nodeError(err)
}

func (r realClusterNodes) HAProxyConfig(ctx context.Context, n *store.Node) ([]byte, error) {
	c, err := r.haproxy(n)
	if err != nil {
		return nil, err
	}
	resp, err := c.GetConfig(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, nodeError(err)
	}
	if resp.GetRedacted() {
		return nil, errors.New("the node masked its configuration: the cluster engine may not apply one there")
	}
	return resp.GetConfig(), nil
}

func (r realClusterNodes) HAProxyValidate(ctx context.Context, n *store.Node, cfg []byte) error {
	c, err := r.haproxy(n)
	if err != nil {
		return err
	}
	resp, err := c.ValidateConfig(ctx, &janusv1alpha1.ValidateConfigRequest{Config: cfg})
	if err != nil {
		return nodeError(err)
	}
	if !resp.GetValid() {
		return fmt.Errorf("haproxy.cfg: %s", strings.Join(resp.GetErrors(), "; "))
	}
	return nil
}

func (r realClusterNodes) HAProxyApply(ctx context.Context, n *store.Node, cfg []byte) error {
	c, err := r.haproxy(n)
	if err != nil {
		return err
	}
	stream, err := c.ApplyConfig(ctx, &janusv1alpha1.ApplyConfigRequest{Config: cfg})
	if err != nil {
		return nodeError(err)
	}
	for {
		p, err := stream.Recv()
		if err == io.EOF {
			return errors.New("the node stopped before saying the configuration was applied")
		}
		if err != nil {
			return nodeError(err)
		}
		switch p.GetStage() {
		case "rejected":
			return fmt.Errorf("haproxy.cfg rejected: %s", p.GetMessage())
		case "done":
			if p.GetAccepted() {
				return nil
			}
			return fmt.Errorf("haproxy.cfg: %s", p.GetMessage())
		}
	}
}

func (r realClusterNodes) Files(ctx context.Context, n *store.Node) ([]*janusv1alpha1.HAProxyFile, error) {
	c, err := r.haproxy(n)
	if err != nil {
		return nil, err
	}
	resp, err := c.FileList(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, nodeError(err)
	}
	return resp.GetFiles(), nil
}

func (r realClusterNodes) FileGet(ctx context.Context, n *store.Node, name string) ([]byte, error) {
	c, err := r.haproxy(n)
	if err != nil {
		return nil, err
	}
	resp, err := c.FileGet(ctx, &janusv1alpha1.FileGetRequest{Name: name})
	if err != nil {
		return nil, nodeError(err)
	}
	return resp.GetContent(), nil
}

func (r realClusterNodes) FilePut(ctx context.Context, n *store.Node, name string, data []byte) error {
	c, err := r.haproxy(n)
	if err != nil {
		return err
	}
	_, err = c.FilePut(ctx, &janusv1alpha1.FilePutRequest{Name: name, Content: data})
	return nodeError(err)
}

func (r realClusterNodes) FileDelete(ctx context.Context, n *store.Node, name string) error {
	c, err := r.haproxy(n)
	if err != nil {
		return err
	}
	_, err = c.FileDelete(ctx, &janusv1alpha1.FileDeleteRequest{Name: name})
	return nodeError(err)
}

func (r realClusterNodes) CertificateUpload(ctx context.Context, n *store.Node, req *janusv1alpha1.CertificateUploadRequest) error {
	c, err := r.haproxy(n)
	if err != nil {
		return err
	}
	_, err = c.CertificateUpload(ctx, req)
	return nodeError(err)
}

func (r realClusterNodes) ModuleConfig(ctx context.Context, n *store.Node, domain string) ([]byte, error) {
	c, err := r.network(n)
	if err != nil {
		return nil, err
	}
	switch domain {
	case "vrrp":
		resp, err := c.VRRPGetConfig(ctx, &emptypb.Empty{})
		return resp.GetConfig(), nodeError(err)
	case "bgp":
		resp, err := c.BGPGetConfig(ctx, &emptypb.Empty{})
		return resp.GetConfig(), nodeError(err)
	case "firewall":
		resp, err := c.FirewallGetRuleset(ctx, &emptypb.Empty{})
		return resp.GetRuleset(), nodeError(err)
	}
	return nil, fmt.Errorf("unknown module %q", domain)
}

func (r realClusterNodes) ModuleApply(ctx context.Context, n *store.Node, domain string, cfg []byte, validateOnly bool) error {
	c, err := r.network(n)
	if err != nil {
		return err
	}
	switch domain {
	case "vrrp":
		_, err := c.VRRPApplyConfig(ctx, &janusv1alpha1.VRRPApplyConfigRequest{Config: cfg, ValidateOnly: validateOnly})
		return nodeError(err)
	case "bgp":
		_, err := c.BGPApplyConfig(ctx, &janusv1alpha1.BGPApplyConfigRequest{Config: cfg, ValidateOnly: validateOnly})
		return nodeError(err)
	case "firewall":
		resp, err := c.FirewallApplyRuleset(ctx, &janusv1alpha1.FirewallApplyRulesetRequest{Ruleset: cfg, ValidateOnly: validateOnly})
		if err != nil {
			return nodeError(err)
		}
		if !resp.GetAccepted() {
			return fmt.Errorf("firewall.nft: %s", strings.Join(resp.GetErrors(), "; "))
		}
		if validateOnly || resp.GetRevertAtUnix() == 0 {
			return nil
		}
		if ok, msg := nodeproxy.ConfirmFirewall(n, time.Unix(resp.GetRevertAtUnix(), 0)); !ok {
			return fmt.Errorf("firewall.nft: not confirmed - %s", msg)
		}
		return nil
	}
	return fmt.Errorf("unknown module %q", domain)
}
