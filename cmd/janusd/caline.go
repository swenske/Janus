package main

import "github.com/swenske/Janus/internal/pki"

// caConsoleLine is the line janusd logs on every boot with its CA's
// SHA-256 (DER, hex) - a contract: an orchestrator that creates a node
// reads the node's serial console for it, then checks the CA the node's
// TrustGet returns against it (docs/private-cloud/first-contact.md). Its
// format doesn't change.
func caConsoleLine(caDER []byte) string {
	return "pki: this node's CA: SHA-256 " + pki.Fingerprint(caDER)
}
