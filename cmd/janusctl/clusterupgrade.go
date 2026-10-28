package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/status"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/clusters"
	"github.com/swenske/Janus/internal/slotimage"
)

// janusctl cluster upgrade (docs/guide/clusters.md "Updating a
// cluster"): the members of a fleet's cluster moved to a release in
// place, one at a time, by the engine the Controller runs too
// (clusters.Engine.Upgrade) - each member drained, its node's own A/B
// update from the bundle at a URL (a release's GitHub assets for a
// version), back on its other slot, its tables learnt, undrained.

// releaseRe is a Janus release's version.
var releaseRe = regexp.MustCompile(`^v\d{4}\.\d{2}\.\d{2}(-\d+)?$`)

// releaseAssets is where a release's bundle is - the default image's.
const releaseAssets = "https://github.com/swenske/Janus/releases/download/"

// bundleClient fetches what janusctl reads of a bundle.
var bundleClient = &http.Client{Timeout: 5 * time.Minute}

func runClusterUpgrade(globalCtx string, args []string) {
	fs := flag.NewFlagSet("cluster upgrade", flag.ExitOnError)
	members := fs.String("members", "", "MEMBER,...: these members, in this order (default: every member, a VRRP cluster's MASTERs last)")
	kexec := fs.Bool("kexec", false, "each member reboots into its new slot through kexec, not the firmware - seconds instead of a server's POST")
	allowSchematic := fs.Bool("allow-schematic-change", false, "the bundle may be another image than the members' - other extensions, HAProxy branch or kernel track")
	insecure := fs.Bool("insecure-skip-signature-check", false, "accept UKIs not signed by a Janus release key - development bundles only")
	sum := fs.String("sha256", "", "rootfs.squashfs's SHA-256 (default: the bundle's rootfs.squashfs.sha256)")
	release := fs.String("version", "", "the release the bundle holds, as the nodes report it (default: read from the bundle's own image)")
	timeout := fs.Duration("timeout", clusters.UpgradeTimeout, "how long each member has to come back on the new release")
	parseAnywhere(fs, args[1:])
	if fs.NArg() != 2 {
		clusterCtlUsage("upgrade")
	}
	base := strings.TrimSuffix(fs.Arg(1), "/")
	target := *release
	if releaseRe.MatchString(base) {
		target, base = base, releaseAssets+base
	}
	if u, err := url.Parse(base); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		log.Fatalf("janusctl cluster upgrade: %q is neither a release (vYYYY.MM.DD[-N]) nor an http(s) URL where a bundle is", fs.Arg(1))
	}
	o := openClusterCtl(globalCtx)
	defer o.close()
	c := o.cluster(fs.Arg(0))
	if *sum == "" {
		if b, err := fetchBundle(base + "/rootfs.squashfs.sha256"); err == nil {
			*sum = strings.Fields(string(b) + " ")[0]
		}
	}
	schematicID := ""
	if target == "" {
		// The release and image the bundle holds, from its own image.json.
		data, err := fetchBundle(base + "/rootfs.squashfs")
		if err != nil {
			log.Fatalf("janusctl cluster upgrade: reading the bundle's image: %v - say its release with -version", err)
		}
		info, err := slotimage.Read(bytes.NewReader(data), int64(len(data)))
		if err != nil || info.Version == "" {
			log.Fatalf("janusctl cluster upgrade: the bundle doesn't say its release (%v) - say it with -version", err)
		}
		target = info.Version
		if *allowSchematic {
			schematicID = info.SchematicID
		}
	}
	var order []string
	if *members != "" {
		order = strings.Split(*members, ",")
	}
	mode := rebootMode(*kexec)
	src := &janusv1alpha1.ImageSource{Reference: base, Sha256: *sum, InsecureSkipSignatureCheck: *insecure, AllowSchematicChange: *allowSchematic}
	opts := clusters.UpgradeOptions{
		Release: target, Schematic: schematicID, Members: order, BackTimeout: *timeout,
		Update: func(ctx context.Context, m clusters.Member) (string, error) {
			return o.upgradeNode(ctx, m, src, mode)
		},
	}
	defer o.exclusive(c.ID)()
	since := time.Now()
	stop := o.follow(c.ID, since)
	ctx, cancel := forCluster(c.ID, time.Duration(len(c.Members)+1)*(*timeout+5*time.Minute))
	defer cancel()
	fmt.Printf("Cluster %s: updating its members to Janus %s from %s, one at a time\n", c.Name, target, base)
	err := o.Upgrade(ctx, c.ID, opts, o.c.User)
	stop()
	check("cluster upgrade", err)
}

// upgradeNode has member m's node update itself from src - checked, and
// reverted unless HAProxy confirms healthy on the new slot -, and
// returns what it says as it reboots.
func (o *clusterCtl) upgradeNode(ctx context.Context, m clusters.Member, src *janusv1alpha1.ImageSource, mode janusv1alpha1.RebootMode) (string, error) {
	conn, err := o.conn(ctx, m)
	if err != nil {
		return "", err
	}
	stream, err := janusv1alpha1.NewLifecycleServiceClient(conn).Upgrade(ctx, &janusv1alpha1.UpgradeRequest{Source: src, WaitForHealth: true, RebootMode: mode})
	if err != nil {
		return "", errors.New(status.Convert(err).Message())
	}
	var last, said string
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// The node reboots once it switched slots: the stream may
			// break then.
			if last == "rebooting" || last == "switching-slot" {
				return said, nil
			}
			return "", errors.New(status.Convert(err).Message())
		}
		last, said = msg.GetStage(), msg.GetMessage()
	}
	if last != "rebooting" && last != "switching-slot" {
		return "", fmt.Errorf("the update ended at %q without rebooting", last)
	}
	return said, nil
}

// follow prints cluster id's journal from since as it grows, until the
// returned stop.
func (o *clusterCtl) follow(id string, since time.Time) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	printed := 0
	flush := func() {
		entries := o.Store.Entries(id, 500)
		slices.Reverse(entries)
		n := 0
		for _, e := range entries {
			if e.Time.Before(since) {
				continue
			}
			if n++; n <= printed {
				continue
			}
			line := e.Message
			if e.Error != "" {
				line += ": " + e.Error
			}
			fmt.Println("  " + line)
			printed = n
		}
	}
	go func() {
		defer close(finished)
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				flush()
				return
			case <-t.C:
				flush()
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// fetchBundle is a bundle file's content.
func fetchBundle(u string) ([]byte, error) {
	resp, err := bundleClient.Get(u) //nolint:gosec // G107: the URL the operator gave
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", u, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 256<<20))
}
