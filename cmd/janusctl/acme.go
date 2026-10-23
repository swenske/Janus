package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/acme"
)

func runACME(conn *grpc.ClientConn, args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	client := janusv1alpha1.NewHAProxyServiceClient(conn)
	c, cancel := ctx()
	defer cancel()
	switch args[0] {
	case "status":
		resp, err := client.ACMEStatus(c, &emptypb.Empty{})
		check("ACMEStatus", err)
		fmt.Println("State:      ", strings.ToLower(strings.TrimPrefix(resp.GetState().String(), "MODULE_STATE_")))
		if resp.GetState() == janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED {
			return
		}
		fmt.Println("CA:         ", resp.GetDirectory())
		fmt.Println("Thumbprint: ", resp.GetAccountThumbprint())
		if resp.GetAccountUri() != "" {
			fmt.Println("Account:    ", resp.GetAccountUri())
		}
		if resp.GetAccountError() != "" {
			fmt.Println("Account error:", resp.GetAccountError())
		}
		fmt.Println("HTTP-01 rule:", resp.GetHttp01Rule())
		if !resp.GetConfigured() {
			fmt.Println("No configuration saved.")
			return
		}
		tw := table("NAME", "STATE", "EXPIRES", "RENEWS", "ISSUER", "DOMAINS")
		for _, s := range resp.GetCertificates() {
			state := s.GetState()
			if s.GetInProgress() {
				state += " (obtaining)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.GetName(), state, day(s.GetNotAfterUnix()), day(s.GetRenewAtUnix()),
				or(s.GetIssuer(), "-"), strings.Join(s.GetDomains(), " "))
		}
		tw.Flush()
		for _, s := range resp.GetCertificates() {
			if s.GetLastError() != "" {
				fmt.Printf("\n%s: last attempt %s failed (%d in a row), next %s:\n  %s\n", s.GetName(), stamp(s.GetLastAttemptUnix()),
					s.GetFailures(), stamp(s.GetNextAttemptUnix()), strings.ReplaceAll(s.GetLastError(), "\n", "\n  "))
			}
		}
	case "get":
		resp, err := client.ACMEGetConfig(c, &emptypb.Empty{})
		check("ACMEGetConfig", err)
		if resp.GetIsDefault() {
			fmt.Fprintln(os.Stderr, "# no configuration saved")
		}
		out, err := acme.Marshal(resp.GetConfig())
		check("marshal", err)
		os.Stdout.Write(out)
	case "check", "apply":
		fs := flag.NewFlagSet("haproxy acme "+args[0], flag.ExitOnError)
		keyFile := fs.String("account-key", "", "an account private key (PEM) to use from now on")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 {
			fmt.Fprintf(os.Stderr, "usage: janusctl haproxy acme %s [-account-key FILE] FILE\n", args[0])
			os.Exit(2)
		}
		data, err := os.ReadFile(fs.Arg(0))
		check("read", err)
		cfg := &janusv1alpha1.ACMEConfig{}
		check("parse "+fs.Arg(0), protojson.Unmarshal(data, cfg))
		req := &janusv1alpha1.ACMEApplyConfigRequest{Config: cfg, ValidateOnly: args[0] == "check"}
		if *keyFile != "" {
			key, err := os.ReadFile(*keyFile)
			check("read", err)
			req.AccountKey = string(key)
		}
		resp, err := client.ACMEApplyConfig(c, req)
		check("ACMEApplyConfig", err)
		if !resp.GetAccepted() {
			printErrors(resp.GetErrors())
			os.Exit(1)
		}
		if args[0] == "check" {
			fmt.Println("valid")
		} else {
			fmt.Println("applied - certificates to obtain are being obtained (haproxy acme status)")
		}
	case "path", "test-path":
		fs := flag.NewFlagSet("haproxy acme "+args[0], flag.ExitOnError)
		user := fs.String("user", "", "proxy: its username")
		passwordFile := fs.String("password-file", "", "proxy: a file holding its password (kept when not given)")
		caFile := fs.String("ca", "", "proxy: the CA (PEM) of an https:// proxy's certificate")
		parseAnywhere(fs, args[1:])
		var path *janusv1alpha1.ACMEPath
		switch fs.Arg(0) {
		case "":
		case "direct":
			path = &janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_DIRECT}
		case "proxy":
			if fs.NArg() != 2 {
				fmt.Fprintf(os.Stderr, "usage: janusctl haproxy acme %s proxy URL [-user NAME] [-password-file FILE] [-ca FILE]\n", args[0])
				os.Exit(2)
			}
			path = &janusv1alpha1.ACMEPath{Mode: janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY, ProxyUrl: fs.Arg(1), ProxyUsername: *user}
			if *passwordFile != "" {
				pw, err := os.ReadFile(*passwordFile)
				check("read", err)
				path.ProxyPassword = strings.TrimRight(string(pw), "\r\n")
			}
			if *caFile != "" {
				ca, err := os.ReadFile(*caFile)
				check("read", err)
				path.ProxyCa = string(ca)
			}
		default:
			fmt.Fprintf(os.Stderr, "janusctl haproxy acme %s: direct or proxy URL, not %q\n", args[0], fs.Arg(0))
			os.Exit(2)
		}
		if args[0] == "test-path" {
			// Fetching the directory may take the node 45 s.
			c, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			resp, err := client.ACMECheckPath(c, &janusv1alpha1.ACMECheckPathRequest{Path: path})
			check("ACMECheckPath", err)
			if !resp.GetOk() {
				fmt.Printf("%s %s: %s\n", resp.GetDirectory(), resp.GetVia(), resp.GetError())
				os.Exit(1)
			}
			fmt.Printf("%s reached %s in %d ms\n", resp.GetDirectory(), resp.GetVia(), resp.GetMilliseconds())
			return
		}
		got, err := client.ACMEGetConfig(c, &emptypb.Empty{})
		check("ACMEGetConfig", err)
		cfg := got.GetConfig()
		if path == nil {
			fmt.Println("The CA is reached", describePath(cfg.GetPath()))
			return
		}
		// The rest as saved: its secrets come back empty, and empty keeps them.
		cfg.Path = path
		resp, err := client.ACMEApplyConfig(c, &janusv1alpha1.ACMEApplyConfigRequest{Config: cfg})
		check("ACMEApplyConfig", err)
		if !resp.GetAccepted() {
			printErrors(resp.GetErrors())
			os.Exit(1)
		}
		fmt.Println("The CA is reached", describePath(path), "from now on - check it with: janusctl haproxy acme test-path")
	case "renew":
		resp, err := client.ACMERenew(c, &janusv1alpha1.ACMERenewRequest{Names: args[1:]})
		check("ACMERenew", err)
		fmt.Println("obtaining:", strings.Join(resp.GetNames(), ", "))
	default:
		fmt.Fprintf(os.Stderr, "janusctl haproxy acme: unknown subcommand %q\n", args[0])
		usage()
		os.Exit(2)
	}
}

func day(unix int64) string {
	if unix == 0 {
		return "-"
	}
	return time.Unix(unix, 0).Format("2006-01-02")
}

func stamp(unix int64) string {
	if unix == 0 {
		return "-"
	}
	return time.Unix(unix, 0).Format(time.RFC3339)
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// describePath says how the node reaches the CA.
func describePath(p *janusv1alpha1.ACMEPath) string {
	if p.GetMode() == janusv1alpha1.ACMEPathMode_ACME_PATH_MODE_PROXY {
		out := "through the proxy " + p.GetProxyUrl()
		if p.GetProxyUsername() != "" {
			out += " (as " + p.GetProxyUsername() + ")"
		}
		return out
	}
	return "directly"
}
