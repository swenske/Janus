package clusters

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// Nodes is what the engine asks of a member's node (docs/cluster.md
// sections 2-4): through the Controller's connections, or janusctl's own
// (GRPC); the tests fake it. The context says for which cluster the
// orchestrator acts - what lets the writes through the members' locks.
type Nodes interface {
	Version(ctx context.Context, m Member) (*janusv1alpha1.VersionResponse, error)
	Healthy(ctx context.Context, m Member) error
	// Probe: m reaches target (host:port) over TCP.
	Probe(ctx context.Context, m Member, target string) error
	CertificateRequest(ctx context.Context, m Member) ([]byte, error)
	ClusterGet(ctx context.Context, m Member) (*janusv1alpha1.ClusterInfo, error)
	ClusterSet(ctx context.Context, m Member, req *janusv1alpha1.ClusterSetRequest) (*janusv1alpha1.ClusterInfo, error)
	ClusterLeave(ctx context.Context, m Member) error
	ClusterStatus(ctx context.Context, m Member) (*janusv1alpha1.ClusterStatusResponse, error)
	ClusterDrain(ctx context.Context, m Member, timeout time.Duration) (string, error)
	ClusterUndrain(ctx context.Context, m Member) error
	// ClusterPull: what came, one line each.
	ClusterPull(ctx context.Context, m Member, from string, items []janusv1alpha1.ClusterItem) ([]string, error)
	RotateTickets(ctx context.Context, m Member, renew bool) error
	HAProxyConfig(ctx context.Context, m Member) ([]byte, error)
	HAProxyValidate(ctx context.Context, m Member, cfg []byte) error
	HAProxyApply(ctx context.Context, m Member, cfg []byte) error
	Files(ctx context.Context, m Member) ([]*janusv1alpha1.HAProxyFile, error)
	FileGet(ctx context.Context, m Member, name string) ([]byte, error)
	FilePut(ctx context.Context, m Member, name string, data []byte) error
	FileDelete(ctx context.Context, m Member, name string) error
	CertificateUpload(ctx context.Context, m Member, req *janusv1alpha1.CertificateUploadRequest) error
	// ModuleConfig and ModuleApply: vrrp, bgp, firewall - the firewall's
	// on trial, confirmed.
	ModuleConfig(ctx context.Context, m Member, domain string) ([]byte, error)
	ModuleApply(ctx context.Context, m Member, domain string, cfg []byte, validateOnly bool) error
}

// GRPC is Nodes over each member's gRPC connection.
type GRPC struct {
	// Conn is member m's connection.
	Conn func(ctx context.Context, m Member) (grpc.ClientConnInterface, error)
	// ConfirmFirewall confirms the ruleset on trial on m until revertAt -
	// over a connection opened after the apply, as the node wants.
	ConfirmFirewall func(ctx context.Context, m Member, revertAt time.Time) error
}

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

func (g GRPC) system(ctx context.Context, m Member) (janusv1alpha1.SystemServiceClient, error) {
	conn, err := g.Conn(ctx, m)
	if err != nil {
		return nil, err
	}
	return janusv1alpha1.NewSystemServiceClient(conn), nil
}

func (g GRPC) haproxy(ctx context.Context, m Member) (janusv1alpha1.HAProxyServiceClient, error) {
	conn, err := g.Conn(ctx, m)
	if err != nil {
		return nil, err
	}
	return janusv1alpha1.NewHAProxyServiceClient(conn), nil
}

func (g GRPC) network(ctx context.Context, m Member) (janusv1alpha1.NetworkServiceClient, error) {
	conn, err := g.Conn(ctx, m)
	if err != nil {
		return nil, err
	}
	return janusv1alpha1.NewNetworkServiceClient(conn), nil
}

func (g GRPC) cluster(ctx context.Context, m Member) (janusv1alpha1.ClusterServiceClient, error) {
	conn, err := g.Conn(ctx, m)
	if err != nil {
		return nil, err
	}
	return janusv1alpha1.NewClusterServiceClient(conn), nil
}

func (g GRPC) Version(ctx context.Context, m Member) (*janusv1alpha1.VersionResponse, error) {
	c, err := g.system(ctx, m)
	if err != nil {
		return nil, err
	}
	v, err := c.Version(ctx, &emptypb.Empty{})
	return v, nodeError(err)
}

func (g GRPC) Healthy(ctx context.Context, m Member) error {
	c, err := g.haproxy(ctx, m)
	if err != nil {
		return err
	}
	_, err = c.ShowInfo(ctx, &emptypb.Empty{})
	return nodeError(err)
}

func (g GRPC) Probe(ctx context.Context, m Member, target string) error {
	c, err := g.system(ctx, m)
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

func (g GRPC) CertificateRequest(ctx context.Context, m Member) ([]byte, error) {
	c, err := g.system(ctx, m)
	if err != nil {
		return nil, err
	}
	resp, err := c.CertificateRequest(ctx, &janusv1alpha1.CertificateRequestRequest{Purpose: janusv1alpha1.CertificatePurpose_CERTIFICATE_PURPOSE_CLUSTER_MEMBER})
	if err != nil {
		return nil, nodeError(err)
	}
	return resp.GetCsr(), nil
}

func (g GRPC) ClusterGet(ctx context.Context, m Member) (*janusv1alpha1.ClusterInfo, error) {
	c, err := g.cluster(ctx, m)
	if err != nil {
		return nil, err
	}
	info, err := c.ClusterGet(ctx, &emptypb.Empty{})
	return info, nodeError(err)
}

func (g GRPC) ClusterSet(ctx context.Context, m Member, req *janusv1alpha1.ClusterSetRequest) (*janusv1alpha1.ClusterInfo, error) {
	c, err := g.cluster(ctx, m)
	if err != nil {
		return nil, err
	}
	info, err := c.ClusterSet(ctx, req)
	return info, nodeError(err)
}

func (g GRPC) ClusterLeave(ctx context.Context, m Member) error {
	c, err := g.cluster(ctx, m)
	if err != nil {
		return err
	}
	_, err = c.ClusterLeave(ctx, &emptypb.Empty{})
	return nodeError(err)
}

func (g GRPC) ClusterStatus(ctx context.Context, m Member) (*janusv1alpha1.ClusterStatusResponse, error) {
	c, err := g.cluster(ctx, m)
	if err != nil {
		return nil, err
	}
	st, err := c.ClusterStatus(ctx, &emptypb.Empty{})
	return st, nodeError(err)
}

func (g GRPC) ClusterDrain(ctx context.Context, m Member, timeout time.Duration) (string, error) {
	c, err := g.cluster(ctx, m)
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

func (g GRPC) ClusterUndrain(ctx context.Context, m Member) error {
	c, err := g.cluster(ctx, m)
	if err != nil {
		return err
	}
	_, err = c.ClusterUndrain(ctx, &emptypb.Empty{})
	return nodeError(err)
}

func (g GRPC) ClusterPull(ctx context.Context, m Member, from string, items []janusv1alpha1.ClusterItem) ([]string, error) {
	c, err := g.cluster(ctx, m)
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

func (g GRPC) RotateTickets(ctx context.Context, m Member, renew bool) error {
	c, err := g.cluster(ctx, m)
	if err != nil {
		return err
	}
	_, err = c.TLSTicketKeysRotate(ctx, &janusv1alpha1.TLSTicketKeysRotateRequest{Renew: renew})
	return nodeError(err)
}

func (g GRPC) HAProxyConfig(ctx context.Context, m Member) ([]byte, error) {
	c, err := g.haproxy(ctx, m)
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

func (g GRPC) HAProxyValidate(ctx context.Context, m Member, cfg []byte) error {
	c, err := g.haproxy(ctx, m)
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

func (g GRPC) HAProxyApply(ctx context.Context, m Member, cfg []byte) error {
	c, err := g.haproxy(ctx, m)
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

func (g GRPC) Files(ctx context.Context, m Member) ([]*janusv1alpha1.HAProxyFile, error) {
	c, err := g.haproxy(ctx, m)
	if err != nil {
		return nil, err
	}
	resp, err := c.FileList(ctx, &emptypb.Empty{})
	if err != nil {
		return nil, nodeError(err)
	}
	return resp.GetFiles(), nil
}

func (g GRPC) FileGet(ctx context.Context, m Member, name string) ([]byte, error) {
	c, err := g.haproxy(ctx, m)
	if err != nil {
		return nil, err
	}
	resp, err := c.FileGet(ctx, &janusv1alpha1.FileGetRequest{Name: name})
	if err != nil {
		return nil, nodeError(err)
	}
	return resp.GetContent(), nil
}

func (g GRPC) FilePut(ctx context.Context, m Member, name string, data []byte) error {
	c, err := g.haproxy(ctx, m)
	if err != nil {
		return err
	}
	_, err = c.FilePut(ctx, &janusv1alpha1.FilePutRequest{Name: name, Content: data})
	return nodeError(err)
}

func (g GRPC) FileDelete(ctx context.Context, m Member, name string) error {
	c, err := g.haproxy(ctx, m)
	if err != nil {
		return err
	}
	_, err = c.FileDelete(ctx, &janusv1alpha1.FileDeleteRequest{Name: name})
	return nodeError(err)
}

func (g GRPC) CertificateUpload(ctx context.Context, m Member, req *janusv1alpha1.CertificateUploadRequest) error {
	c, err := g.haproxy(ctx, m)
	if err != nil {
		return err
	}
	_, err = c.CertificateUpload(ctx, req)
	return nodeError(err)
}

func (g GRPC) ModuleConfig(ctx context.Context, m Member, domain string) ([]byte, error) {
	c, err := g.network(ctx, m)
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

func (g GRPC) ModuleApply(ctx context.Context, m Member, domain string, cfg []byte, validateOnly bool) error {
	c, err := g.network(ctx, m)
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
		if err := g.ConfirmFirewall(ctx, m, time.Unix(resp.GetRevertAtUnix(), 0)); err != nil {
			return fmt.Errorf("firewall.nft: not confirmed - %v", err)
		}
		return nil
	}
	return fmt.Errorf("unknown module %q", domain)
}
