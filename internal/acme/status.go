package acme

import (
	"os"
	"time"

	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
)

// Status is the extension's state: the account, and each certificate's.
func (m *Manager) Status() *janusv1alpha1.ACMEStatusResponse {
	out := &janusv1alpha1.ACMEStatusResponse{State: janusv1alpha1.ModuleState_MODULE_STATE_NOT_ENABLED}
	if !m.Available() {
		return out
	}
	out.State = janusv1alpha1.ModuleState_MODULE_STATE_RUNNING
	out.Http01Rule = HTTP01Rule
	out.CertificatesDir = CertDir
	_, err := os.Stat(CertDir)
	if err != nil {
		out.State = janusv1alpha1.ModuleState_MODULE_STATE_ERROR
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	out.AccountThumbprint = m.thumb
	if m.cfg == nil {
		out.Directory = ResolveDirectory("")
		return out
	}
	out.Configured = true
	dir := ResolveDirectory(m.cfg.Account.GetDirectory())
	out.Directory = dir
	if a := m.st.Accounts[m.accountKey(dir, m.thumb)]; a != nil {
		out.AccountUri, out.AccountError, out.TermsUrl = a.URI, a.Error, a.TermsURL
	}
	now := time.Now()
	for _, c := range m.cfg.Certificates {
		cs := m.certState(c.Name)
		s := &janusv1alpha1.ACMECertificateStatus{
			Name: c.Name, Domains: c.Domains, Path: certPath(c.Name), State: "pending",
			InProgress: m.inProgress == c.Name || m.forced[c.Name],
			LastError:  cs.LastError, Failures: uint32(cs.Failures),
		}
		s.LastAttemptUnix = unix(cs.LastAttempt)
		s.LastSuccessUnix = unix(cs.LastSuccess)
		s.NextAttemptUnix = unix(cs.NextAttempt)
		if data, err := os.ReadFile(certPath(c.Name)); err == nil {
			if b, err := parseBundle(data); err == nil && !b.Placeholder {
				s.ServedDomains = b.Leaf.DNSNames
				s.Issuer = b.Leaf.Issuer.CommonName
				s.NotBeforeUnix, s.NotAfterUnix = b.Leaf.NotBefore.Unix(), b.Leaf.NotAfter.Unix()
				s.Serial = serialHex(b.Leaf)
				reason, at := m.due(c, cs, dir, now)
				s.RenewAtUnix = unix(at)
				switch {
				case now.After(b.Leaf.NotAfter):
					s.State = "expired"
				case reason != "":
					s.State = "due"
				default:
					s.State = "valid"
				}
			}
		}
		out.Certificates = append(out.Certificates, s)
	}
	return out
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
