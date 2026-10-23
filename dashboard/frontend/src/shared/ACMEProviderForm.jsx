import { CheckCircle2, Plus, Trash2, Waypoints } from 'lucide-react'
import { useState } from 'react'
import { Card } from './ui.jsx'

// A DNS provider for Let's Encrypt's DNS-01 challenge, as a form: a
// node's own (its page, Apps › Let's Encrypt) or the Controller's, which
// it orders its nodes' certificates with (its Let's Encrypt tab). Saved
// settings come back without their values: left empty, they're kept.

// The DNS providers janus-acme has, with the settings they usually need
// (lego's names - docs/letsencrypt.md lists them all).
const PROVIDERS = {
  gandiv5: ['GANDIV5_PERSONAL_ACCESS_TOKEN'],
  ovh: ['OVH_ENDPOINT', 'OVH_APPLICATION_KEY', 'OVH_APPLICATION_SECRET', 'OVH_CONSUMER_KEY'],
  cloudflare: ['CLOUDFLARE_DNS_API_TOKEN'],
  route53: ['AWS_ACCESS_KEY_ID', 'AWS_SECRET_ACCESS_KEY', 'AWS_REGION', 'AWS_HOSTED_ZONE_ID'],
  pdns: ['PDNS_API_URL', 'PDNS_API_KEY'],
  rfc2136: ['RFC2136_NAMESERVER', 'RFC2136_TSIG_KEY', 'RFC2136_TSIG_SECRET', 'RFC2136_TSIG_ALGORITHM'],
  hetzner: ['HETZNER_API_TOKEN'],
  digitalocean: ['DO_AUTH_TOKEN'],
  scaleway: ['SCALEWAY_API_TOKEN', 'SCALEWAY_PROJECT_ID'],
  ionos: ['IONOS_API_KEY'],
  infomaniak: ['INFOMANIAK_ACCESS_TOKEN'],
  desec: ['DESEC_TOKEN'],
  httpreq: ['HTTPREQ_ENDPOINT', 'HTTPREQ_USERNAME', 'HTTPREQ_PASSWORD'],
}

const lines = (text) =>
  text
    .split(/[\s,]+/)
    .map((l) => l.trim())
    .filter(Boolean)

export default function ProviderForm({ provider, names, busy, onCancel, onSave }) {
  const [name, setName] = useState(provider?.name || '')
  const [type, setType] = useState(provider?.type || 'gandiv5')
  // Saved settings come back without their values: empty keeps them.
  const [settings, setSettings] = useState(() =>
    provider ? Object.keys(provider.settings || {}).map((k) => ({ key: k, value: '', saved: true })) : PROVIDERS.gandiv5.map((k) => ({ key: k, value: '' })),
  )
  const [resolvers, setResolvers] = useState((provider?.resolvers || []).join(' '))
  const [wait, setWait] = useState(provider?.propagation_wait_seconds ? String(provider.propagation_wait_seconds) : '')
  const changeType = (t) => {
    setType(t)
    if (!provider) setSettings(PROVIDERS[t].map((k) => ({ key: k, value: '' })))
  }
  const set = (i, change) => setSettings(settings.map((s, j) => (j === i ? { ...s, ...change } : s)))
  const used = settings.filter((s) => s.key.trim() && (s.value || s.saved))
  const nameTaken = !provider && names.includes(name.trim())
  const save = () => {
    const p = { name: name.trim(), type, settings: Object.fromEntries(used.map((s) => [s.key.trim(), s.value])) }
    const r = lines(resolvers)
    if (r.length) p.resolvers = r
    if (Number(wait) > 0) p.propagation_wait_seconds = Number(wait)
    onSave(p)
  }
  return (
    <div style={{ marginTop: '1rem' }}>
      <Card className="data-title" title={provider ? `Edit ${provider.name}` : 'Add a DNS provider'} icon={Waypoints}>
        <div className="grid grid-2">
          <label className="field">
            <span>Name - what certificates refer to it by</span>
            <input className="mono" value={name} disabled={!!provider} onChange={(e) => setName(e.target.value)} placeholder="gandi" />
          </label>
          <label className="field">
            <span>Type</span>
            <select value={type} onChange={(e) => changeType(e.target.value)} disabled={!!provider}>
              {Object.keys(PROVIDERS).map((t) => (
                <option key={t}>{t}</option>
              ))}
            </select>
          </label>
        </div>
        <div className="field" style={{ marginTop: '0.6rem' }}>
          <span className="field-label">Settings - lego's names for {type}; see the guide for every option</span>
          {settings.map((s, i) => (
            <div className="row" key={i} style={{ marginBottom: '0.3rem' }}>
              <input
                className="mono"
                style={{ width: '45%' }}
                value={s.key}
                disabled={s.saved}
                onChange={(e) => set(i, { key: e.target.value.toUpperCase() })}
                placeholder="SETTING_NAME"
              />
              <input
                className="mono grow"
                type="password"
                autoComplete="off"
                value={s.value}
                onChange={(e) => set(i, { value: e.target.value })}
                placeholder={s.saved ? 'saved - leave empty to keep' : 'value'}
              />
              <button className="small" title="Remove" onClick={() => setSettings(settings.filter((_, j) => j !== i))}>
                <Trash2 size={13} />
              </button>
            </div>
          ))}
          <div>
            <button className="small" onClick={() => setSettings([...settings, { key: `${PROVIDERS[type][0].split('_')[0]}_`, value: '' }])}>
              <Plus size={13} /> Add a setting
            </button>
          </div>
        </div>
        <div className="grid grid-2" style={{ marginTop: '0.6rem' }}>
          <label className="field">
            <span>Resolvers for the propagation check (optional)</span>
            <input
              className="mono"
              value={resolvers}
              onChange={(e) => setResolvers(e.target.value)}
              placeholder="1.1.1.1 9.9.9.9 - public ones with split-horizon DNS"
            />
          </label>
          <label className="field">
            <span>Or wait instead of checking (seconds, optional)</span>
            <input type="number" min={0} max={3600} value={wait} onChange={(e) => setWait(e.target.value)} placeholder="check that the record is visible" />
          </label>
        </div>
        {nameTaken && <div className="notice">A provider is already named {name.trim()}.</div>}
        <div className="row" style={{ marginTop: '0.8rem' }}>
          <button className="primary" disabled={busy || !name.trim() || nameTaken || !used.length} onClick={save}>
            <CheckCircle2 size={15} /> Save…
          </button>
          <button onClick={onCancel}>Cancel</button>
        </div>
      </Card>
    </div>
  )
}
