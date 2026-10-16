package logship

import (
	janusv1alpha1 "github.com/swenske/Janus/gen/janus/v1alpha1"
	"github.com/swenske/Janus/internal/tlstrust"
)

// FromProto is a configuration as the API carries it
// (SystemService.LogForwardingSet), as the shipper takes it.
func FromProto(p *janusv1alpha1.LogForwardingConfig) Config {
	c := Config{Labels: p.GetLabels(), AccessLogsOnDisk: p.GetAccessLogsOnDisk(), ManagedBy: p.GetManagedBy()}
	for _, t := range p.GetTargets() {
		c.Targets = append(c.Targets, TargetFromProto(t))
	}
	return c
}

// TargetFromProto is one target as the API carries it.
func TargetFromProto(t *janusv1alpha1.LogTarget) Target {
	out := Target{Name: t.GetName(), Sources: t.GetSources(), Disabled: t.GetDisabled()}
	if sl := t.GetSyslog(); sl != nil {
		out.Syslog = &SyslogTarget{Address: sl.GetAddress(), Transport: sl.GetTransport(), Format: sl.GetFormat(), Framing: sl.GetFraming(), Facility: sl.GetFacility()}
	}
	if es := t.GetElasticsearch(); es != nil {
		out.Elasticsearch = &ElasticsearchTarget{URL: es.GetUrl(), Namespace: es.GetNamespace(), APIKey: es.GetApiKey(), Username: es.GetUsername(), Password: es.GetPassword(), Gzip: es.GetGzip()}
	}
	if tr := t.GetTls(); tr != nil {
		out.TLS = tlstrust.Trust{CAPEM: tr.GetCaPem(), Fingerprint: tr.GetFingerprint(), ServerName: tr.GetServerName(), ClientCertPEM: tr.GetClientCertPem(), ClientKeyPEM: tr.GetClientKeyPem()}
	}
	return out
}

// ToProto is c as the API carries it.
func ToProto(c Config) *janusv1alpha1.LogForwardingConfig {
	p := &janusv1alpha1.LogForwardingConfig{Labels: c.Labels, AccessLogsOnDisk: c.AccessLogsOnDisk, ManagedBy: c.ManagedBy}
	for _, t := range c.Targets {
		p.Targets = append(p.Targets, TargetToProto(t))
	}
	return p
}

// TargetToProto is one target as the API carries it.
func TargetToProto(t Target) *janusv1alpha1.LogTarget {
	pt := &janusv1alpha1.LogTarget{Name: t.Name, Sources: t.Sources, Disabled: t.Disabled}
	if sl := t.Syslog; sl != nil {
		pt.Syslog = &janusv1alpha1.SyslogTarget{Address: sl.Address, Transport: sl.Transport, Format: sl.Format, Framing: sl.Framing, Facility: sl.Facility}
	}
	if es := t.Elasticsearch; es != nil {
		pt.Elasticsearch = &janusv1alpha1.ElasticsearchTarget{Url: es.URL, Namespace: es.Namespace, ApiKey: es.APIKey, Username: es.Username, Password: es.Password, Gzip: es.Gzip}
	}
	if t.TLS != (tlstrust.Trust{}) {
		pt.Tls = &janusv1alpha1.TLSTrust{CaPem: t.TLS.CAPEM, Fingerprint: t.TLS.Fingerprint, ServerName: t.TLS.ServerName, ClientCertPem: t.TLS.ClientCertPEM, ClientKeyPem: t.TLS.ClientKeyPEM}
	}
	return pt
}
