package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/rbac"
	"github.com/swenske/Janus/internal/support"
)

// certRole is the role the certificate janusctl calls with carries
// (main sets it): "" or a scoped certificate's when unknown here.
var certRole string

// runSupport writes the node's diagnostic bundle (internal/support):
// what the certificate's role may read, never a secret.
func runSupport(conn *grpc.ClientConn, args []string) {
	fs := flag.NewFlagSet("system support", flag.ExitOnError)
	out := fs.String("o", "", "the bundle's file - janus-support-<hostname>-<time>.tar.gz by default, - for stdout")
	_ = fs.Parse(args)
	if fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: janusctl system support [-o FILE]")
		os.Exit(2)
	}
	c, cancel := interruptible()
	defer cancel()
	host := ""
	hctx, hcancel := context.WithTimeout(c, 10*time.Second)
	if h, err := janusv1alpha1.NewSystemServiceClient(conn).Hostname(hctx, &emptypb.Empty{}); err == nil {
		host = h.GetHostname()
	}
	hcancel()
	name := *out
	if name == "" {
		name = fmt.Sprintf("janus-support-%s-%s.tar.gz", firstOr(host, "node"), time.Now().UTC().Format("20060102T150405Z"))
	}
	var w io.Writer = os.Stdout
	if name != "-" {
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // logs are in it
		if err != nil {
			log.Fatalf("janusctl: %v", err)
		}
		defer f.Close()
		w = f
	}
	col := &support.Collector{Conn: conn, Node: host, Role: certRole, By: "janusctl " + version, Allowed: allowedFor(certRole)}
	m, err := col.Write(c, w)
	if err != nil {
		log.Fatalf("janusctl: %v", err)
	}
	var left []string
	for _, s := range m.Sections {
		if s.Status == "skipped" || s.Status == "failed" {
			left = append(left, s.File)
		}
	}
	where := name
	if name == "-" {
		where = "stdout"
	}
	fmt.Fprintf(os.Stderr, "janusctl: the diagnostic bundle is in %s: %d files, %d left out (MANIFEST.json says why)\n", where, len(m.Sections)-len(left), len(left))
}

// allowedFor is what a role may call, as internal/rbac says - nil
// (everything: the node refuses what it must) for a role janusctl
// can't judge, a scoped certificate's.
func allowedFor(role string) func(string) bool {
	if !slices.Contains([]string{"os:admin", "os:operator", "os:reader"}, role) {
		return nil
	}
	return func(method string) bool {
		return rbac.Allowed("/janus.v1alpha1."+strings.TrimPrefix(method, "/janus.v1alpha1."), []string{role})
	}
}
