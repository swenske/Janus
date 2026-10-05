import { ChevronDown, Lock, RotateCcw, Upload } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { call, postJSON } from './call.js'
import { Badge, Card, ErrorBox, useAction, useConfirm } from './shared/ui.jsx'

// The certificate the Controller's page is served with
// (dashboard/backend/controller_tls.go): its own self-signed identity by
// default, or one of the organization's - uploaded here, or given as
// files (-tls-cert/-tls-key). Nodes register against the self-signed
// identity whatever the page uses.

const SOURCE = {
  'self-signed': { label: 'self-signed', tone: undefined },
  uploaded: { label: 'your certificate', tone: 'ok' },
  files: { label: 'from -tls-cert', tone: 'info' },
}

function day(t) {
  return t ? new Date(t).toLocaleDateString() : ''
}

function colonHex(h) {
  return (h || '').toUpperCase().match(/.{2}/g)?.join(':') || ''
}

function Details({ info }) {
  return (
    <>
      <dl className="kv">
        <dt>Names</dt>
        <dd className="mono">{info.names.join(', ') || '-'}</dd>
        <dt>Subject</dt>
        <dd className="mono">{info.subject}</dd>
        <dt>Issuer</dt>
        <dd className="mono">{info.issuer}</dd>
        <dt>Valid</dt>
        <dd>
          {day(info.not_before)} to {day(info.not_after)}
        </dd>
        <dt>Chain</dt>
        <dd>
          {info.chain} certificate{info.chain === 1 ? '' : 's'}
        </dd>
        <dt>SHA-256</dt>
        <dd className="mono small">{colonHex(info.fingerprint)}</dd>
      </dl>
      {info.warnings.map((w) => (
        <div key={w} className="notice warn small">
          {w.charAt(0).toUpperCase() + w.slice(1)}.
        </div>
      ))}
    </>
  )
}

export default function HTTPSCertificate() {
  const [info, setInfo] = useState(null)
  const [error, setError] = useState(null)
  const [open, setOpen] = useState(false)
  const [editing, setEditing] = useState(false)
  const [bundle, setBundle] = useState('')
  const [candidate, setCandidate] = useState(null)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const load = useCallback(() => {
    call('/api/controller/tls').then(setInfo, setError)
  }, [])
  useEffect(load, [load])
  if (error) return <ErrorBox error={error} />
  if (!info) return null

  const addFile = async (e) => {
    const texts = await Promise.all([...e.target.files].map((f) => f.text()))
    setBundle((b) => [b, ...texts].filter(Boolean).join('\n'))
    setCandidate(null)
    e.target.value = ''
  }
  const check = async (e) => {
    e.preventDefault()
    const c = await run(() => postJSON('/api/controller/tls/check', { pem: bundle }))
    if (c) setCandidate(c)
  }
  const serve = async () => {
    const ok = await confirm({
      title: 'Serve the page with this certificate?',
      body: (
        <>
          <p>New connections to this page, janusctl and Terraform get it at once - reload the page to see it. Nodes keep registering against the self-signed identity.</p>
          <p className="muted">
            janusctl contexts that pinned the self-signed certificate sign in again: <span className="mono">janusctl login</span> takes a certificate the machine trusts, or{' '}
            <span className="mono">-controller-ca</span> its CA.
          </p>
        </>
      ),
      action: 'Serve it',
    })
    if (!ok) return
    const r = await run(() => postJSON('/api/controller/tls', { pem: bundle }, 'PUT'), 'The page has its certificate')
    if (r) {
      setInfo(r)
      setEditing(false)
      setBundle('')
      setCandidate(null)
    }
  }
  const revert = async () => {
    const ok = await confirm({
      title: 'Back to the self-signed certificate?',
      body: <p>The uploaded certificate and its key are deleted. Browsers that don&apos;t trust the self-signed certificate warn again.</p>,
      action: 'Go back',
      danger: true,
    })
    if (!ok) return
    const r = await run(() => call('/api/controller/tls', { method: 'DELETE' }), 'Back to the self-signed certificate')
    if (r) setInfo(r)
  }

  const src = SOURCE[info.source] || SOURCE['self-signed']
  return (
    <Card
      title="HTTPS certificate"
      icon={Lock}
      actions={
        <button className="small ghost" onClick={() => setOpen(!open)}>
          {open ? 'Hide' : 'Show'} <ChevronDown size={14} style={{ transform: open ? 'rotate(180deg)' : undefined }} />
        </button>
      }
    >
      <div className="row" style={{ flexWrap: 'wrap' }}>
        <Badge tone={src.tone}>{src.label}</Badge>
        <span className="muted small">
          {info.names.slice(0, 3).join(', ')}
          {info.names.length > 3 ? ` +${info.names.length - 3}` : ''} · until {day(info.not_after)}
        </span>
        {info.warnings.length > 0 && <Badge tone="warn">{info.warnings.length === 1 ? '1 warning' : `${info.warnings.length} warnings`}</Badge>}
      </div>
      {open && (
        <div className="stack" style={{ marginTop: '0.8rem' }}>
          <Details info={info} />
          {info.from_files ? (
            <div className="muted small">Given by -tls-cert/-tls-key on the Controller: replace those files to change it - they&apos;re read again within 30 seconds.</div>
          ) : editing ? (
            <form className="stack" onSubmit={check}>
              <label className="field">
                <span>The certificate, then its chain, and its private key (PEM)</span>
                <textarea
                  rows={8}
                  className="mono"
                  spellCheck={false}
                  value={bundle}
                  onChange={(e) => (setBundle(e.target.value), setCandidate(null))}
                  placeholder="-----BEGIN CERTIFICATE-----"
                  required
                />
              </label>
              {candidate && (
                <div className="stack">
                  <strong>The certificate given</strong>
                  <Details info={candidate} />
                </div>
              )}
              <div className="row">
                <label className="button small">
                  Load PEM file(s)
                  <input type="file" multiple hidden onChange={addFile} />
                </label>
                <span className="grow" />
                <button type="button" onClick={() => (setEditing(false), setBundle(''), setCandidate(null))}>
                  Cancel
                </button>
                {candidate ? (
                  <button type="button" className="primary" onClick={serve} disabled={busy}>
                    <Upload size={15} /> Serve it
                  </button>
                ) : (
                  <button className="primary" disabled={busy || !bundle.trim()}>
                    Check
                  </button>
                )}
              </div>
            </form>
          ) : (
            <div className="row">
              <button className="small" onClick={() => setEditing(true)}>
                <Upload size={14} /> {info.source === 'uploaded' ? 'Replace it…' : 'Use a certificate of your own…'}
              </button>
              {info.source === 'uploaded' && (
                <button className="small ghost" onClick={revert} disabled={busy}>
                  <RotateCcw size={14} /> Back to self-signed
                </button>
              )}
            </div>
          )}
          <div className="muted small">
            Nodes register against the Controller&apos;s self-signed identity on its registration port whatever this page uses: a node provisioned with it keeps working.
          </div>
        </div>
      )}
    </Card>
  )
}
