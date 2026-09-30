import { Copy, KeyRound } from 'lucide-react'
import { useState } from 'react'
import { download, postJSON } from '../api.js'
import { Card, PageHeader, useAction, useToast } from '../../shared/ui.jsx'

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

export default function Access() {
  const [role, setRole] = useState('os:reader')
  const [format, setFormat] = useState('pfx')
  const [name, setName] = useState('')
  const [password, setPassword] = useState('')
  const [pem, setPem] = useState(null)
  const [busy, run] = useAction()

  const issue = (e) => {
    e.preventDefault()
    setPem(null)
    const body = { role, format, password, name }
    run(
      () => (format === 'pfx' ? download('/api/pki/client', 'client.pfx', body) : postJSON('/api/pki/client', body).then(setPem)),
      format === 'pfx' ? (r) => `Saved ${r.name}` : 'Certificate issued',
    )
  }

  return (
    <>
      <PageHeader title="Access" subtitle="Issue client certificates for this node" />
      <div className="grid grid-2">
        <Card title="New client certificate" icon={KeyRound}>
          <form className="stack" onSubmit={issue}>
            <div className="grid grid-2">
              <label className="field">
                <span>Role</span>
                <select value={role} onChange={(e) => setRole(e.target.value)}>
                  <option value="os:reader">Reader - status and metrics only</option>
                  <option value="os:admin">Admin - full control</option>
                </select>
              </label>
              <label className="field">
                <span>Format</span>
                <select value={format} onChange={(e) => setFormat(e.target.value)}>
                  <option value="pfx">.pfx (browser / Controller import)</option>
                  <option value="pem">PEM files (janusctl)</option>
                </select>
              </label>
            </div>
            <label className="field">
              <span>Label (used in the file name)</span>
              <input value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. alice-laptop" />
            </label>
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
        <Card title="About node access">
          <div className="stack">
            <p style={{ margin: 0 }}>
              Every Janus node has its own certificate authority. A certificate issued here is signed by this node's CA and valid for one year; it works with{' '}
              <code>janusctl</code> (PEM) and, as a <code>.pfx</code>, for opening this page in a browser or adding the node to another Controller.
            </p>
            <p className="muted" style={{ margin: 0 }}>
              The private key is generated on the node and passed through the Controller to your browser once - it isn't stored. There is no revocation yet: keep issued
              certificates as safe as a password.
            </p>
          </div>
        </Card>
      </div>
      {pem && (
        <Card title="Issued certificate" icon={KeyRound} className="">
          <div className="stack">
            <PemBlock label="CA certificate" value={pem.ca} file="ca.crt" />
            <PemBlock label="Client certificate" value={pem.crt} file="client.crt" />
            <PemBlock label="Private key" value={pem.key} file="client.key" />
            <div className="muted small">
              Use with: <code>janusctl -ca ca.crt -cert client.crt -key client.key -endpoint {window.location.hostname}:9505 version</code>
            </div>
          </div>
        </Card>
      )}
    </>
  )
}
