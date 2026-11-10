package pki

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
)

// ConsoleOID marks a client certificate a node's own CA issued at the
// node's console - a rescue window's (PairingService/Rescue), whose code
// only the console shows - and never through the node's API: no API call
// issues one (GenerateClientConfiguration, LocalCARotate, a cluster's or
// a transfer's certificates don't). Not critical: a client that doesn't
// know it ignores it.
//
// What a node lets only someone at its console - or the fleet's root
// itself, the recovery kit - do takes it: carry its identity away
// (StateExport, StateExportGrant with it), forget its fleet
// (TrustReset). A certificate an admin had the node issue through its
// API is its own CA's too, but it says nothing about the console: an
// admin certificate of an issuing CA, twelve hours, would otherwise turn
// into one of the node's own, a year, and into its identity (security
// review 2026-11-05 #28, the follow-up of #5).
//
// Provisional OID under the example arc, like ScopeOID (2.999.1.1).
var ConsoleOID = asn1.ObjectIdentifier{2, 999, 1, 2}

// consoleExtension is the mark: a UTF8String naming where it came from.
func consoleExtension() pkix.Extension {
	value, _ := asn1.MarshalWithParams("console", "utf8") // a constant: never fails
	return pkix.Extension{Id: ConsoleOID, Value: value}
}

// IsConsole reports whether c carries the console mark. Only who signed
// it - the node's own CA, checked by the caller - makes it mean anything.
func IsConsole(c *x509.Certificate) bool {
	if c == nil {
		return false
	}
	for _, e := range c.Extensions {
		if e.Id.Equal(ConsoleOID) {
			return true
		}
	}
	return false
}
