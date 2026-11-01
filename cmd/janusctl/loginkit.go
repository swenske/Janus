package main

import (
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/swenske/Janus/internal/pki"
)

// loginKit is `janusctl login -kit KIT`: the Controller down, a
// context's nodes reached for an hour with a certificate the fleet's
// root signs - its key from the recovery kit, opened here. Nothing is
// asked of the Controller and nothing changes on the nodes: they accept a
// certificate their fleet's root signs (internal/pki/fleet.go), their
// bundle stays the Controller's, and once it's back `janusctl login`
// signs in as before. The nodes are the context's, as the Controller
// listed them at the last sign-in.
func loginKit(name string, ctx *cliContext, kitPath, role string) error {
	if len(ctx.Nodes) == 0 {
		return fmt.Errorf("context %q knows no node: the nodes come from the Controller at a sign-in (a fleet without one: janusctl fleet recover -kit)", name)
	}
	if role != pki.RoleAdmin && role != pki.RoleOperator && role != pki.RoleReader {
		return fmt.Errorf("-role %q: os:admin, os:operator or os:reader", role)
	}
	dir := contextDir(name)
	certFile := filepath.Join(dir, "cert.pem")
	_, root, err := openKitFile(kitPath)
	if err != nil {
		return err
	}
	if err := sameFleet(certFile, root.Cert); err != nil {
		return err
	}
	var pub crypto.PublicKey
	if ctx.SSHKey != "" {
		// The context's TLS key is its SSH key (nodeTLS): the certificate
		// is for it.
		k, err := openSSHKey(ctx.SSHKey)
		if err != nil {
			return err
		}
		defer k.Close()
		pub = k.tls.Public()
	} else {
		key, err := contextKey(dir)
		if err != nil {
			return err
		}
		pub = key.Public()
	}
	leaf, err := root.IssueFor(pub, pki.IssueOptions{CommonName: kitUser(ctx.User), Roles: []string{role}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, Validity: kitCertValidity})
	if err != nil {
		return err
	}
	b, _ := pem.Decode(leaf)
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(certFile, append(leaf, root.CertPEM...), 0o600); err != nil {
		return err
	}
	ctx.Role, ctx.Expires, ctx.Kit = role, cert.NotAfter, true
	return nil
}

// kitUser is the name a recovery kit's certificate carries - what the
// nodes log: the account, marked.
func kitUser(user string) string {
	if user == "" {
		return "kit"
	}
	return "kit:" + user
}

// sameFleet checks that root is the fleet of the context whose
// certificate is at certFile: that root signed its issuer, or is in its
// chain.
func sameFleet(certFile string, root *x509.Certificate) error {
	data, err := os.ReadFile(certFile)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("this context has no certificate to check the kit against: it never signed in to its Controller")
	}
	if err != nil {
		return err
	}
	for rest := data; ; {
		var b *pem.Block
		if b, rest = pem.Decode(rest); b == nil {
			break
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			continue
		}
		if c.Equal(root) || c.CheckSignatureFrom(root) == nil {
			return nil
		}
	}
	return errors.New("this kit is another fleet's: the context's certificates come from another root")
}
