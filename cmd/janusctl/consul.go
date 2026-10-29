package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

type fileFlags map[string]string

func (f fileFlags) String() string { return "" }
func (f fileFlags) Set(v string) error {
	name, path, ok := strings.Cut(v, "=")
	if !ok {
		path, name = v, filepath.Base(v)
	}
	f[name] = path
	return nil
}

func runConsul(conn *grpc.ClientConn, args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	client := janusv1alpha1.NewNetworkServiceClient(conn)
	c, cancel := ctx()
	defer cancel()
	switch args[0] {
	case "status":
		resp, err := client.ConsulStatus(c, &emptypb.Empty{})
		check("ConsulStatus", err)
		fmt.Println("State:     ", strings.ToLower(strings.TrimPrefix(resp.GetState().String(), "MODULE_STATE_")))
		if resp.GetState() == janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED {
			return
		}
		fmt.Println("Saved:     ", map[bool]string{true: "yes", false: "no"}[resp.GetConfigured()])
		fmt.Println("Service:   ", resp.GetServiceState())
		if resp.GetServiceError() != "" {
			fmt.Println("Last exit: ", resp.GetServiceError())
		}
		if resp.GetError() != "" {
			fmt.Println("Agent:     ", resp.GetError())
		}
		if resp.GetNodeName() == "" {
			return
		}
		role := map[bool]string{true: "server", false: "client"}[resp.GetServer()]
		fmt.Printf("Node:       %s (%s, %s, datacenter %s, Consul %s)\n", resp.GetNodeName(), resp.GetNodeId(), role, resp.GetDatacenter(), resp.GetVersion())
		fmt.Println("Leader:    ", or(resp.GetLeader(), "none"))
		if len(resp.GetMembers()) > 0 {
			tw := table("MEMBER", "ADDRESS", "STATUS", "ROLE", "DATACENTER")
			for _, m := range resp.GetMembers() {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.GetName(), m.GetAddress(), m.GetStatus(), m.GetRole(), m.GetDatacenter())
			}
			tw.Flush()
		}
	case "get":
		resp, err := client.ConsulGetConfig(c, &emptypb.Empty{})
		check("ConsulGetConfig", err)
		if resp.GetIsDefault() {
			fmt.Fprintln(os.Stderr, "# no configuration saved")
		}
		for _, f := range resp.GetFiles() {
			fmt.Fprintln(os.Stderr, "# file:", f)
		}
		os.Stdout.Write(resp.GetConfig())
	case "check", "apply":
		fs := flag.NewFlagSet("network consul "+args[0], flag.ExitOnError)
		files := fileFlags{}
		fs.Var(files, "file", "NAME=PATH: a file the configuration names, as /run/janus/consul/files/NAME (repeatable)")
		only := fs.Bool("only-files", false, "remove the saved files not given with -file")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 {
			fmt.Fprintf(os.Stderr, "usage: janusctl network consul %s [-file NAME=PATH]... FILE\n", args[0])
			os.Exit(2)
		}
		cfg, err := os.ReadFile(fs.Arg(0))
		check("read", err)
		req := &janusv1alpha1.ConsulApplyConfigRequest{Config: cfg, ValidateOnly: args[0] == "check", Files: map[string][]byte{}}
		if !*only {
			saved, err := client.ConsulGetConfig(c, &emptypb.Empty{})
			check("ConsulGetConfig", err)
			for _, f := range saved.GetFiles() {
				req.Files[f] = nil // keeps the saved one
			}
		}
		for name, path := range files {
			data, err := os.ReadFile(path)
			check("read", err)
			req.Files[name] = data
		}
		resp, err := client.ConsulApplyConfig(c, req)
		check("ConsulApplyConfig", err)
		if !resp.GetAccepted() {
			printErrors(resp.GetErrors())
			os.Exit(1)
		}
		if args[0] == "check" {
			fmt.Println("valid")
		} else {
			fmt.Println("applied - the agent restarts with it")
		}
	case "managed":
		resp, err := client.ConsulManagedGet(c, &emptypb.Empty{})
		check("ConsulManagedGet", err)
		if resp.GetState() == janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED {
			fmt.Println("State:      not enabled")
			return
		}
		m := resp.GetManaged()
		if m.GetManagedBy() == "" {
			fmt.Println("Managed:    no - the agent runs on the operator's configuration alone")
			return
		}
		yes := map[bool]string{true: "set", false: "none"}
		fmt.Println("Managed by:", m.GetManagedBy())
		fmt.Println("Datacenter:", m.GetDatacenter())
		fmt.Println("Node name: ", m.GetNodeName())
		fmt.Println("Join:      ", strings.Join(m.GetRetryJoin(), ", "))
		fmt.Println("Advertise: ", or(m.GetAdvertiseAddr(), "the node's main address"))
		fmt.Println("Gossip key:", yes[m.GetHasEncrypt()])
		fmt.Println("ACL token: ", yes[m.GetHasAgentToken()])
		fmt.Println("RPC CA:    ", map[bool]string{true: "set (TLS, auto_encrypt)", false: "none"}[m.GetCaPem() != ""])
		var meta []string
		for k, v := range m.GetNodeMeta() {
			meta = append(meta, k+"="+v)
		}
		sort.Strings(meta)
		fmt.Println("Node meta: ", strings.Join(meta, ", "))
		sv := m.GetServices()
		var on []string
		for _, t := range []struct {
			name string
			on   bool
		}{{"janus", sv.GetJanus()}, {"exporters", sv.GetExporters()}, {"frontends", sv.GetFrontends()}, {"urls", sv.GetUrls()}} {
			if t.on {
				on = append(on, t.name)
			}
		}
		fmt.Println("Registers: ", or(strings.Join(on, ", "), "nothing"))
		synced := "never"
		if resp.GetSyncedUnix() > 0 {
			synced = time.Unix(resp.GetSyncedUnix(), 0).Local().Format(time.DateTime)
		}
		fmt.Printf("Services:   %d registered, last synced %s", resp.GetRegistered(), synced)
		if resp.GetMaintenance() {
			fmt.Print(", in maintenance")
		}
		fmt.Println()
		if resp.GetSyncError() != "" {
			fmt.Println("Sync error:", resp.GetSyncError())
		}
	case "services":
		resp, err := client.ConsulStatus(c, &emptypb.Empty{})
		check("ConsulStatus", err)
		if resp.GetState() == janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED {
			fmt.Println("State:      not enabled")
			return
		}
		if resp.GetError() != "" {
			fmt.Fprintln(os.Stderr, "Agent:", resp.GetError())
		}
		tw := table("ID", "NAME", "ADDRESS", "STATUS", "BY", "META")
		for _, s := range resp.GetServices() {
			by := map[bool]string{true: "janusd", false: "config"}[s.GetManaged()]
			addr := s.GetAddress()
			if s.GetPort() > 0 {
				addr = net.JoinHostPort(addr, fmt.Sprint(s.GetPort()))
			}
			meta := s.GetMeta()["url"]
			if meta == "" {
				meta = s.GetMeta()["frontend"]
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.GetId(), s.GetName(), addr, or(s.GetStatus(), "-"), by, or(meta, "-"))
		}
		tw.Flush()
	default:
		fmt.Fprintf(os.Stderr, "janusctl network consul: unknown subcommand %q\n", args[0])
		usage()
		os.Exit(2)
	}
}
