import { Copy, KeyRound, RefreshCcw, ShieldCheck } from 'lucide-react'
import { useState } from 'react'
import { download, postJSON } from '../api.js'
import { dateTime } from '../format.js'
import { usePoll } from '../hooks.jsx'
import { useMay } from '../may.js'
import { Badge, Card, ErrorBox, PageHeader, useAction, useConfirm, useToast } from '../../shared/ui.jsx'

function PemBlock({ label, value, file }) {
  const toast = useToast()
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value)
      toast(`${label} copied`)
    } catch {
      toast('Copy failed - select the text instead', 'warn')
    }
  }
  const save = () => {
    const a = document.createElement('a')
    a.href = URL.createObjectURL(new Blob([value], { type: 'application/x-pem-file' }))
    a.download = file
    a.click()
  }
  return (
    <div className="stack" style={{ gap: '0.3rem' }}>
      <div className="spread">
        <strong>{label}</strong>
        <span className="row">
          <button className="small" onClick={copy}>
            <Copy size={13} /> Copy
          </button>
          <button className="small" onClick={save}>
            Save {file}
          </button>
        </span>
      </div>
      <pre className="code" style={{ maxHeight: '11rem' }}>
        {value}
      </pre>
    </div>
  )
}

function short(fp) {
  return fp ? `${fp.slice(0, 16)}…` : ''
}

function CertLine({ cert }) {
  if (!cert) return <span className="muted">–</span>
  return (
    <span title={cert.fingerprint}>
      {cert.subject || 'unnamed'} · <span className="mono small">{short(cert.fingerprint)}</span> · until {dateTime(cert.not_after)}
    </span>
  )
}

// Trust: who the node lets in - its fleet's issuing CAs, its own CA.
function Trust({ trust }) {
  if (trust.error) return <ErrorBox error={trust.error} />
  const t = trust.data
  if (!t) return null
  return (
    <Card title="Who this node lets in" icon={ShieldCheck}>
      <dl className="kv">
        <dt>Fleet</dt>
        <dd>
          {t.root ? (
            <>
              <CertLine cert={t.root} /> <Badge tone="ok">trusted</Badge>
            </>
          ) : (
            <span className="muted">none - only its own CA lets in</span>
          )}
        </dd>
        {t.root && (
          <>
            <dt>Trust bundle</dt>
            <dd>
              version {t.bundle_version}
              {t.bundle_issued && <> · signed {dateTime(t.bundle_issued)}</>}
            </dd>
            <dt>Issuing CAs</dt>
            <dd>
              {t.issuing_cas.length === 0 ? (
                <span className="muted">none</span>
              ) : (
                t.issuing_cas.map((c) => (
                  <div key={c.fingerprint}>
                    <CertLine cert={c} />
                  </div>
                ))
              )}
            </dd>
          </>
        )}
        <dt>Its own CA</dt>
        <dd>
          <CertLine cert={t.local_ca} />
        </dd>
      </dl>
      <p className="muted small" style={{ marginBottom: 0 }}>
        The fleet&apos;s certificates - this Controller&apos;s, janusctl&apos;s after <code>janusctl login</code> - are short-lived and issued per account. The node&apos;s own CA
        is the way in without the Controller: the first boot&apos;s admin credential and the certificates issued below.
      </p>
    </Card>
  )
}

function pemOf(type, buf) {
  const b64 = btoa(String.fromCharCode(...new Uint8Array(buf)))
  return `-----BEGIN ${type}-----\n${b64.match(/.{1,64}/g).join('\n')}\n-----END ${type}-----\n`
}

// RotateCA replaces the node's own CA: every certificate it issued stops
// working. The new admin credential's key is made in this browser and
// never leaves it - or the node prints one on its console.
function RotateCA({ trust, node, onDone }) {
  const [where, setWhere] = useState('browser')
  const [result, setResult] = useState(null)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const onFleet = trust.data?.controller_on_fleet
  const rotate = async () => {
    const ok = await confirm({
      title: "Replace this node's own CA?",
      body: (
        <>
          <p>
            Every certificate it issued stops working at once: the first boot&apos;s admin credential, and every one issued on this page or with{' '}
            <code>janusctl pki generate-client-config</code>. The fleet&apos;s - this Controller, janusctl after a login - keep working.
          </p>
          <p>
            {where === 'browser'
              ? 'The new admin credential is for a key made in this browser: save its three files as soon as they show.'
              : "The new admin credential is printed on the node's console - its serial port and screen - once."}
          </p>
        </>
      ),
      action: 'Replace the CA',
      danger: true,
      typeToConfirm: node?.name,
    })
    if (!ok) return
    setResult(null)
    await run(async () => {
      if (where === 'console') {
        const r = await postJSON('/api/access/rotate-ca', { console: true })
        setResult({ ...r, console: true })
        return
      }
      const kp = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, true, ['sign', 'verify'])
      const pub = pemOf('PUBLIC KEY', await crypto.subtle.exportKey('spki', kp.publicKey))
      const key = pemOf('PRIVATE KEY', await crypto.subtle.exportKey('pkcs8', kp.privateKey))
      const r = await postJSON('/api/access/rotate-ca', { admin_public_key: pub })
      setResult({ ...r, admin_key: key })
    }, 'The node has a new CA')
    onDone()
  }
  return (
    <Card title="Replace the node's own CA" icon={RefreshCcw}>
      <div className="stack">
        <p style={{ margin: 0 }}>
          After a credential its CA issued leaked - or to end those issued before the fleet: they all stop working, and a new admin credential is made for the way in
          without the Controller.
        </p>
        {!onFleet ? (
          <div className="notice warn">
            This Controller reaches the node with a credential its own CA issued: replacing the CA would cut it off. Secure the fleet first (the Controller&apos;s Nodes page).
          </div>
        ) : (
          <>
            <label className="check">
              <input type="radio" name="rotate-where" checked={where === 'browser'} onChange={() => setWhere('browser')} /> The new admin credential here - its key made in this
              browser, never sent
            </label>
            <label className="check">
              <input type="radio" name="rotate-where" checked={where === 'console'} onChange={() => setWhere('console')} /> Printed on the node&apos;s console
            </label>
            <div>
              <button className="danger" disabled={busy || !trust.data} onClick={rotate}>
                <RefreshCcw size={15} /> Replace the CA…
              </button>
            </div>
          </>
        )}
        {result && (
          <div className="stack">
            <div className="notice">
              New CA <span className="mono small">{short(result.ca?.fingerprint)}</span>. The Controller follows it by itself (the old CA signed the new one).
              {result.console && ' The admin credential is on the node\'s console.'}
            </div>
            <PemBlock label="CA certificate" value={result.ca_cert} file="ca.crt" />
            {!result.console && (
              <>
                <PemBlock label="Admin certificate" value={result.admin_cert} file="admin.crt" />
                <PemBlock label="Admin private key" value={result.admin_key} file="admin.key" />
                <div className="muted small">
                  Use with: <code>janusctl -ca ca.crt -cert admin.crt -key admin.key -endpoint {node?.address || 'NODE:9505'} version</code> - keep the key like a password:
                  it&apos;s nowhere else.
                </div>
              </>
            )}
          </div>
        )}
      </div>
    </Card>
  )
}

// Validities offered, in seconds - 0 is the node's most, one year.
const VALIDITIES = [
  [3600, '1 hour'],
  [86400, '1 day'],
  [7 * 86400, '7 days'],
  [30 * 86400, '30 days'],
  [90 * 86400, '90 days'],
  [0, '1 year'],
]

export default function Access() {
  const node = usePoll('/api/node', { every: 0 }).data
  const trust = usePoll('/api/access/trust', { every: 0 })
  const may = useMay()
  const [role, setRole] = useState('os:reader')
  const [format, setFormat] = useState('pfx')
  const [name, setName] = useState('')
  const [ttl, setTTL] = useState(0)
  const [password, setPassword] = useState('')
  const [pem, setPem] = useState(null)
  const [busy, run] = useAction()

  const issue = (e) => {
    e.preventDefault()
    setPem(null)
    const body = { role, format, password, name: name.trim(), ttl_seconds: ttl }
    run(
      () => (format === 'pfx' ? download('/api/pki/client', 'client.pfx', body) : postJSON('/api/pki/client', body).then(setPem)),
      format === 'pfx' ? (r) => `Saved ${r.name}` : 'Certificate issued',
    )
  }

  return (
    <>
      <PageHeader title="Access" subtitle="Who this node lets in, and its own credentials" />
      <div className="grid grid-2">
        <Trust trust={trust} />
        {may('AccessService/LocalCARotate') && <RotateCA trust={trust} node={node} onDone={trust.reload} />}
      </div>
      {may('SystemService/GenerateClientConfiguration') && (
      <div className="grid grid-2" style={{ marginTop: '1rem' }}>
        <Card title="New client certificate" icon={KeyRound}>
          <form className="stack" onSubmit={issue}>
            <div className="grid grid-2">
              <label className="field">
                <span>Role</span>
                <select value={role} onChange={(e) => setRole(e.target.value)}>
                  <option value="os:reader">Reader - status and metrics only</option>
                  <option value="os:operator">Operator - runs HAProxy and services, not the node&apos;s setup</option>
                  <option value="os:admin">Admin - full control</option>
                </select>
              </label>
              <label className="field">
                <span>Format</span>
                <select value={format} onChange={(e) => setFormat(e.target.value)}>
                  <option value="pfx">.pfx (to add the node to a Controller)</option>
                  <option value="pem">PEM files (janusctl)</option>
                </select>
              </label>
            </div>
            <div className="grid grid-2">
              <label className="field">
                <span>Name (who it's for)</span>
                <input
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="e.g. alice-laptop"
                  maxLength={64}
                  pattern="[A-Za-z0-9][A-Za-z0-9 ._@:+\-]*"
                  title="Letters, digits, spaces and ._@:+- - starting with a letter or digit"
                />
              </label>
              <label className="field">
                <span>Valid for</span>
                <select value={ttl} onChange={(e) => setTTL(Number(e.target.value))}>
                  {VALIDITIES.map(([seconds, label]) => (
                    <option key={seconds} value={seconds}>
                      {label}
                    </option>
                  ))}
                </select>
              </label>
            </div>
            {format === 'pfx' && (
              <label className="field">
                <span>.pfx password</span>
                <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} placeholder="protects the private key inside the file" />
              </label>
            )}
            {role === 'os:admin' && <div className="notice warn">An admin certificate gives full control of this node, including reboot, reset and issuing more certificates.</div>}
            <div>
              <button className="primary" disabled={busy}>
                <KeyRound size={15} /> Issue certificate
              </button>
            </div>
          </form>
        </Card>
        <Card title="About these certificates">
          <div className="stack">
            <p style={{ margin: 0 }}>
              Signed by this node&apos;s own CA, with the name you give it, valid for the time you choose - one year at most. For <code>janusctl</code> without the Controller
              (PEM), or, as an admin <code>.pfx</code>, to add the node to another Controller. Day to day, <code>janusctl login</code> gets short-lived fleet certificates
              instead.
            </p>
            <p className="muted" style={{ margin: 0 }}>
              The private key is made on the node and passed through the Controller to your browser once - it isn&apos;t stored. Only replacing the node&apos;s CA (above)
              revokes them: keep them as safe as a password.
            </p>
          </div>
        </Card>
      </div>
      )}
      {pem && (
        <Card title="Issued certificate" icon={KeyRound} className="">
          <div className="stack">
            <PemBlock label="CA certificate" value={pem.ca} file="ca.crt" />
            <PemBlock label="Client certificate" value={pem.crt} file="client.crt" />
            <PemBlock label="Private key" value={pem.key} file="client.key" />
            <div className="muted small">
              Use with: <code>janusctl -ca ca.crt -cert client.crt -key client.key -endpoint {node?.address || 'NODE:9505'} version</code>
            </div>
          </div>
        </Card>
      )}
    </>
  )
}
