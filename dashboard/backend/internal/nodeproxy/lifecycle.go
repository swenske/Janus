// Remote-update Phase 5 (MVP, 2026-09-29): the per-node "Update" relay -
// same relay principle as ops.go's own HAProxy handlers (dial the real
// node with its stored service credential, never the browser's client
// certificate), fronting LifecycleService.Upgrade/UploadReleaseFile
// instead of HAProxyService.
//
// Deliberately no automatic "here's the latest release" detection yet
// (see docs/companion-site-builder-scope.md's own tracking note) - that
// needs real, tagged GitHub Releases to compare against, which don't
// exist yet. This MVP instead lets the operator supply the update
// target directly, either an http(s):// URL (LifecycleService.Upgrade's
// own node-initiated fetch mode) or by uploading a release bundle's 4
// files from their own machine (relayed to the node via
// UploadReleaseFile, for nodes that can't dial out at all - the same
// choice janusctl's own "lifecycle upgrade" vs. "lifecycle
// upload-release" commands already expose, just from the browser).
// Automatic detection can be layered on top of this once Phase 4 lands,
// without disturbing either code path here.
package nodeproxy

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"

	"github.com/swenske/Janus/dashboard/backend/internal/store"
)

// upgradeUploadTimeout bounds the whole upload-then-upgrade flow, not
// just one file - generous for a real release bundle (rootfs.squashfs
// alone can be several 10s of MiB) over a slow link.
const upgradeUploadTimeout = 10 * time.Minute

func registerLifecycleRoutes(mux *http.ServeMux, node *store.Node) {
	mux.HandleFunc("/api/lifecycle/upgrade-url", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Reference            string `json:"reference"`
			SHA256               string `json:"sha256"`
			WaitForHealth        bool   `json:"wait_for_health"`
			HealthTimeoutSeconds uint32 `json:"health_timeout_seconds"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		if req.Reference == "" {
			http.Error(w, "reference is required", http.StatusBadRequest)
			return
		}

		conn, err := dialNode(node)
		if err != nil {
			http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
			return
		}
		defer conn.Close()

		runUpgrade(w, r, janusv1alpha1.NewLifecycleServiceClient(conn), req.Reference, req.SHA256, req.WaitForHealth, req.HealthTimeoutSeconds)
	})

	mux.HandleFunc("/api/lifecycle/upgrade-upload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleUpgradeUpload(w, r, node)
	})
}

// releaseFormFields maps this handler's own browser-form field names to
// the fixed release-bundle filenames LifecycleService.UploadReleaseFile
// requires (its own doc comment) - the form's field names are
// underscore-separated for HTML-attribute friendliness, the real
// filenames aren't.
var releaseFormFields = map[string]string{
	"rootfs_squashfs": "rootfs.squashfs",
	"rootfs_verity":   "rootfs.verity",
	"uki_a":           "uki-a.efi",
	"uki_b":           "uki-b.efi",
}

// handleUpgradeUpload streams a browser-submitted multipart form
// straight through to the node - never buffers a whole file in memory
// at once, the same reasoning cmd/janusctl's own "lifecycle
// upload-release" chunks its reads for, just driven by an HTTP request
// body instead of local files.
func handleUpgradeUpload(w http.ResponseWriter, r *http.Request, node *store.Node) {
	reader, err := r.MultipartReader()
	if err != nil {
		http.Error(w, fmt.Sprintf("parse multipart form: %v", err), http.StatusBadRequest)
		return
	}

	conn, err := dialNode(node)
	if err != nil {
		http.Error(w, fmt.Sprintf("dial node: %v", err), http.StatusBadGateway)
		return
	}
	defer conn.Close()
	client := janusv1alpha1.NewLifecycleServiceClient(conn)

	ctx, cancel := context.WithTimeout(r.Context(), upgradeUploadTimeout)
	defer cancel()

	var sha256Value string
	var waitForHealth bool
	var healthTimeoutSeconds uint32
	var stagingDir string
	seen := map[string]bool{}

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("read multipart form: %v", err), http.StatusBadGateway)
			return
		}

		switch name := part.FormName(); name {
		case "sha256":
			sha256Value = readFormValue(part)
		case "wait_for_health":
			waitForHealth = readFormValue(part) == "true"
		case "health_timeout_seconds":
			if n, err := strconv.Atoi(readFormValue(part)); err == nil && n > 0 {
				healthTimeoutSeconds = uint32(n)
			}
		case "rootfs_squashfs", "rootfs_verity", "uki_a", "uki_b":
			dir, err := relayReleaseFile(ctx, client, releaseFormFields[name], part)
			part.Close()
			if err != nil {
				http.Error(w, fmt.Sprintf("upload %s: %v", releaseFormFields[name], err), http.StatusBadGateway)
				return
			}
			stagingDir = dir
			seen[name] = true
			continue
		}
		part.Close()
	}

	for field, filename := range releaseFormFields {
		if !seen[field] {
			http.Error(w, fmt.Sprintf("missing required file: %s (%s)", field, filename), http.StatusBadRequest)
			return
		}
	}

	runUpgrade(w, r, client, stagingDir, sha256Value, waitForHealth, healthTimeoutSeconds)
}

func readFormValue(part *multipart.Part) string {
	data, _ := io.ReadAll(part)
	return strings.TrimSpace(string(data))
}

// relayReleaseFile streams one multipart part's bytes to the node via
// UploadReleaseFile, chunked the same way cmd/janusctl's own
// "lifecycle upload-release" does, and returns the resulting staging
// directory.
func relayReleaseFile(ctx context.Context, client janusv1alpha1.LifecycleServiceClient, filename string, src io.Reader) (string, error) {
	const chunkSize = 512 * 1024

	stream, err := client.UploadReleaseFile(ctx)
	if err != nil {
		return "", err
	}

	buf := make([]byte, chunkSize)
	first := true
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			req := &janusv1alpha1.UploadReleaseFileRequest{Chunk: buf[:n]}
			if first {
				req.Filename = filename
				first = false
			}
			if err := stream.Send(req); err != nil {
				break // real error retrievable via CloseAndRecv below, not Send's own bare io.EOF
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", fmt.Errorf("read %s: %w", filename, readErr)
		}
	}
	if first {
		_ = stream.Send(&janusv1alpha1.UploadReleaseFileRequest{Filename: filename})
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return "", err
	}
	return resp.GetStagingDir(), nil
}

// runUpgrade drains Upgrade's whole progress stream server-side and
// returns just the final stage/message as plain JSON - same
// simplification ops.go's own handleApplyConfig already applies for the
// identical reason: a browser polling one REST endpoint doesn't need
// the intermediate stages, and Upgrade's own connection dies with the
// node's reboot regardless, well before any health confirmation or
// possible revert (see internal/api/lifecycle.go's own doc comment).
func runUpgrade(w http.ResponseWriter, r *http.Request, client janusv1alpha1.LifecycleServiceClient, reference, sha256Value string, waitForHealth bool, healthTimeoutSeconds uint32) {
	ctx, cancel := context.WithTimeout(r.Context(), upgradeUploadTimeout)
	defer cancel()

	stream, err := client.Upgrade(ctx, &janusv1alpha1.UpgradeRequest{
		Source:               &janusv1alpha1.ImageSource{Reference: reference, Sha256: sha256Value},
		WaitForHealth:        waitForHealth,
		HealthTimeoutSeconds: healthTimeoutSeconds,
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("Upgrade: %v", err), http.StatusBadGateway)
		return
	}

	var last *janusv1alpha1.UpgradeResponse
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, fmt.Sprintf("Upgrade stream: %v", err), http.StatusBadGateway)
			return
		}
		last = msg
	}
	if last == nil {
		http.Error(w, "Upgrade returned no progress messages", http.StatusBadGateway)
		return
	}

	writeJSONBody(w, http.StatusOK, struct {
		Stage   string `json:"stage"`
		Message string `json:"message"`
	}{Stage: last.GetStage(), Message: last.GetMessage()})
}
