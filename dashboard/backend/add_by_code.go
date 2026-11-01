package main

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/swenske/Janus/dashboard/backend/internal/audit"
	"github.com/swenske/Janus/dashboard/backend/internal/labels"
	"github.com/swenske/Janus/dashboard/backend/internal/store"
	"github.com/swenske/Janus/internal/pairing"
	"github.com/swenske/Janus/internal/pki"
)

// pairTimeout bounds a pairing: the node answers at once, or never.
const pairTimeout = 15 * time.Second

// pairNode is pairing.Pair - a variable for tests.
var pairNode = pairing.Pair

// addByCode makes looking for a node's CA among the nodes and recording
// it one step: two calls with the same code (a double click) add it once.
var addByCode sync.Mutex

// addNodeByCode adds a node by the pairing code its console shows
// (internal/pairing, docs/internals/trust.md "The pairing window"): its
// window - the API's host, port 9506, unless pairing_address says - is
// checked against the code, given this Controller's fleet, and answers
// with its CA, which the node's record pins. Nothing secret is kept: the
// node trusts the fleet from then on, and the Controller reaches it with
// its fleet certificate, like a node admitted with no key.
func (a *app) addNodeByCode(w http.ResponseWriter, r *http.Request, req addNodeRequest) {
	var rootPEM, bundle []byte
	ok := false
	if a.fleet != nil { // nil only in tests
		rootPEM, bundle, _, ok = a.fleet.Trust()
	}
	if !ok {
		http.Error(w, "this Controller's fleet isn't set up yet: secure it first (Nodes › Secure your fleet) - a node paired by code trusts the fleet", http.StatusConflict)
		return
	}
	if err := labels.Check(req.Labels); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	host, _, err := net.SplitHostPort(req.Address)
	if err != nil {
		http.Error(w, fmt.Sprintf("address %q: host:port, the node's API (port 9505)", req.Address), http.StatusBadRequest)
		return
	}
	pairAddr := req.PairingAddress
	if pairAddr == "" {
		pairAddr = net.JoinHostPort(host, pairing.DefaultPort)
	}
	if _, err := pairing.ParseCode(req.Code); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), pairTimeout)
	defer cancel()
	_, ca, err := pairNode(ctx, pairAddr, req.Code, rootPEM, bundle)
	if err != nil {
		code, msg := pairingError(pairAddr, err)
		http.Error(w, msg, code)
		return
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})
	// The node answers the same call again for a while (a lost answer):
	// the same node twice is one record.
	addByCode.Lock()
	defer addByCode.Unlock()
	for _, n := range a.store.List() {
		if b, _ := pem.Decode(n.CA()); b != nil && bytes.Equal(b.Bytes, ca.Raw) {
			http.Error(w, fmt.Sprintf("this node is already here, as %s", n.Name), http.StatusConflict)
			return
		}
	}
	node := &store.Node{
		Name:      req.Name,
		Address:   req.Address,
		CACertPEM: caPEM,
		Fleet:     true,
		Labels:    req.Labels,
	}
	if err := a.store.Add(node); err != nil {
		// The node answers the same call again for internal/api.PairReplay.
		http.Error(w, fmt.Sprintf("the node is paired - it trusts the fleet - but recording it failed: %v - add it again within 30 seconds: the node answers the same code that long", err), http.StatusInternalServerError)
		return
	}
	a.trust.Kick()
	a.logfwd.Poke()
	a.consul.Poke()
	noteAuditSubject(r, audit.KindNode, node.ID, node.Name)
	log.Printf("node added by its pairing code: %s (%s), its CA SHA-256 %s", node.Name, node.Address, pki.Fingerprint(ca.Raw))
	writeJSON(w, http.StatusCreated, struct {
		ID string `json:"id"`
	}{ID: node.ID})
}

// pairingError is the answer to a failed pairing: the node's own words
// when it answered - a wrong code (tries left), its window closed -, a
// window that isn't the code's, or no window at all.
func pairingError(addr string, err error) (int, string) {
	st, _ := status.FromError(err)
	msg := st.Message()
	switch {
	case strings.Contains(msg, "isn't the one its code names") || strings.Contains(msg, "isn't signed by its own CA"):
		return http.StatusBadRequest, fmt.Sprintf("%s: %s - check the code, and that %s is that node", addr, cut(msg), addr)
	case st.Code() == codes.PermissionDenied:
		return http.StatusBadRequest, msg
	case st.Code() == codes.FailedPrecondition:
		return http.StatusConflict, msg
	case st.Code() == codes.InvalidArgument:
		return http.StatusBadGateway, "the node refused this Controller's fleet: " + msg
	case errors.Is(err, context.DeadlineExceeded) || st.Code() == codes.DeadlineExceeded || st.Code() == codes.Unavailable:
		return http.StatusBadGateway, fmt.Sprintf("no pairing window answers at %s (%s) - a node shows a code only until it's paired or provisioned, and port %s must be reachable from the Controller", addr, cut(msg), pairing.DefaultPort)
	}
	return http.StatusBadGateway, fmt.Sprintf("pairing with %s: %s", addr, msg)
}

// cut keeps a transport error's last words - gRPC wraps the handshake's
// in "connection error: desc = \"transport: ...\"".
func cut(msg string) string {
	if i := strings.LastIndex(msg, ": "); i >= 0 && strings.Contains(msg, "transport:") {
		return strings.TrimSuffix(msg[i+2:], `"`)
	}
	return msg
}
