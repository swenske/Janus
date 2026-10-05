import { CheckCircle2, Copy, Download, KeyRound, ShieldCheck, Upload } from 'lucide-react'
import { useState } from 'react'
import { postJSON } from './call.js'
import { Badge, Card, useAction, useConfirm, useToast } from './shared/ui.jsx'

// The fleet's setup (dashboard/backend/fleet.go): create it, store the
// recovery kit and its passphrase, give both back - only then does the
// root's key leave the Controller and do the nodes change anything.

export function Step({ n, done, active, title, children }) {
  return (
    <li className={`fleet-step${done ? ' done' : ''}${active ? ' active' : ''}`}>
      <span className="fleet-step-n">{done ? <CheckCircle2 size={18} /> : n}</span>
      <div className="stack" style={{ gap: '0.5rem', minWidth: 0 }}>
        <strong>{title}</strong>
        {active && children}
      </div>
    </li>
  )
}

function MasterKeyNotice({ fleet }) {
  if (!fleet?.master_key_beside_data) return null
  return (
    <div className="notice warn small">
      The Controller's master key - which seals the fleet's issuing key - is in its data directory: a copy of that directory holds both. Set{' '}
      <code>JANUS_CONTROLLER_MASTER_KEY_FILE</code> to a file outside it (see the Controller's README).
    </div>
  )
}

// FleetSetup is shown until the fleet is ready.
export function FleetSetup({ fleet, onChanged }) {
  const [passphrase, setPassphrase] = useState(null)
  const [kitFile, setKitFile] = useState('')
  const [saved, setSaved] = useState(false)
  const [kit, setKit] = useState('')
  const [typed, setTyped] = useState('')
  const [busy, run] = useAction()
  const toast = useToast()
  const confirm = useConfirm()
  if (!fleet || fleet.state === 'ready') return null
  const pending = fleet.state === 'pending'

  const create = () =>
    run(async () => {
      const r = await postJSON('/api/fleet/setup')
      setPassphrase(r.passphrase)
      setKitFile(r.kit_file)
      setSaved(false)
      await onChanged()
    })
  const startOver = async () => {
    const ok = await confirm({
      title: 'Start over?',
      body: <p>A new fleet is made, with a new recovery kit and passphrase. The kit downloaded before won't be any use: delete it.</p>,
      action: 'Start over',
    })
    if (ok) create()
  }
  const download = () =>
    run(async () => {
      const resp = await fetch('/api/fleet/recovery-kit')
      if (!resp.ok) throw new Error((await resp.text()) || resp.statusText)
      const a = document.createElement('a')
      a.href = URL.createObjectURL(new Blob([await resp.text()], { type: 'text/plain' }))
      a.download = kitFile || 'janus-recovery-kit.age'
      a.click()
    })
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(passphrase)
      toast('Passphrase copied')
    } catch {
      toast('Copy failed - select the text instead', 'warn')
    }
  }
  const readKit = (e) => {
    const f = e.target.files?.[0]
    if (f) f.text().then(setKit)
  }
  const prove = (e) => {
    e.preventDefault()
    run(async () => {
      await postJSON('/api/fleet/confirm', { kit, passphrase: typed })
      await onChanged()
    }, 'The fleet is ready - its root key is gone from this Controller')
  }

  return (
    <Card title="Secure your fleet" icon={ShieldCheck} className="fleet-setup">
      <div className="stack">
        <p style={{ margin: 0 }}>
          Today the Controller reaches each node with a credential it got when the node was added, kept here for good. A fleet replaces them: your nodes trust it, and the
          Controller reaches them with short-lived certificates of its own.
        </p>
        <p className="muted" style={{ margin: 0 }}>
          The fleet's root key doesn't stay on the Controller: it goes in a recovery kit you keep, encrypted with a passphrase. You'll need both to renew the fleet's keys, or to
          recover a lost Controller - nothing else. Nothing changes for your nodes until step 3.
        </p>
        <ol className="fleet-steps">
          <Step n={1} done={pending} active={!pending} title="Create the fleet">
            <div>
              <button className="primary" disabled={busy} onClick={create}>
                <ShieldCheck size={15} /> Create the fleet
              </button>
            </div>
          </Step>
          <Step n={2} done={pending && saved} active={pending && !saved} title="Store the recovery kit and its passphrase">
            {passphrase ? (
              <div className="stack" style={{ gap: '0.6rem' }}>
                <div className="fleet-passphrase">
                  <span className="mono">{passphrase}</span>
                  <button type="button" className="small" onClick={copy}>
                    <Copy size={13} /> Copy
                  </button>
                </div>
                <div className="muted small">
                  The passphrase is shown this once. Store it and the kit together - a password manager is ideal: the kit is text, a secure note holds it.
                </div>
                <div className="row">
                  <button className="primary" disabled={busy} onClick={download}>
                    <Download size={15} /> Download the recovery kit
                  </button>
                  <button onClick={() => setSaved(true)}>I stored both</button>
                </div>
              </div>
            ) : (
              <div className="stack" style={{ gap: '0.6rem' }}>
                <div className="muted small">The passphrase was shown when the fleet was created. Kept it? Download the kit, if you haven't, and go on - lost it? Start over.</div>
                <div className="row">
                  <button disabled={busy} onClick={download}>
                    <Download size={15} /> Download the recovery kit
                  </button>
                  <button onClick={() => setSaved(true)}>I stored both</button>
                  <button className="ghost" disabled={busy} onClick={startOver}>
                    Start over
                  </button>
                </div>
              </div>
            )}
          </Step>
          <Step n={3} active={pending && saved} title="Prove you kept them">
            <form className="stack" style={{ gap: '0.6rem' }} onSubmit={prove}>
              <div className="muted small">
                Give the kit and its passphrase back: once they open, the root key is deleted from this Controller, and your nodes are brought to trust the fleet.
              </div>
              <label className="field">
                <span>Recovery kit</span>
                <input type="file" accept=".age,text/plain" onChange={readKit} />
              </label>
              <label className="field">
                <span>Passphrase</span>
                <input
                  value={typed}
                  onChange={(e) => setTyped(e.target.value)}
                  placeholder="XXXX-XXXX-XXXX-XXXX-XXXX-XXXX"
                  className="mono"
                  autoComplete="off"
                  spellCheck={false}
                />
              </label>
              <div className="row">
                <button className="primary" disabled={busy || !kit || !typed}>
                  <Upload size={15} /> Confirm
                </button>
                <button type="button" className="ghost" onClick={() => setSaved(false)}>
                  Back
                </button>
              </div>
            </form>
          </Step>
        </ol>
        <MasterKeyNotice fleet={fleet} />
      </div>
    </Card>
  )
}

const day = (s) => (s ? new Date(s).toLocaleDateString() : '–')

// FleetCard is the fleet once ready.
export function FleetCard({ fleet, nodes }) {
  if (!fleet || fleet.state !== 'ready') return null
  const states = Object.values(fleet.nodes || {})
  const trusted = states.filter((s) => s.state === 'trusted').length
  return (
    <Card title="Fleet" icon={KeyRound}>
      <div className="stack">
        <dl className="kv small">
          <dt>Nodes</dt>
          <dd>
            {trusted} of {nodes?.length ?? states.length} trust the fleet
          </dd>
          <dt>Root</dt>
          <dd>
            <span className="mono" title={fleet.root_fingerprint}>
              {fleet.root_fingerprint?.slice(0, 16)}…
            </span>{' '}
            · its key is in your recovery kit, until {day(fleet.root_not_after)}
          </dd>
          <dt>Issuing CA</dt>
          <dd>
            until {day(fleet.issuing_not_after)} - bundle version {fleet.bundle_version}
          </dd>
          <dt>Set up</dt>
          <dd>{day(fleet.confirmed)}</dd>
        </dl>
        <MasterKeyNotice fleet={fleet} />
      </div>
    </Card>
  )
}

// TrustBadge says how the Controller reaches a node, once the fleet is
// ready.
export function TrustBadge({ fleet, nodeID }) {
  if (!fleet || fleet.state !== 'ready') return null
  const t = fleet.nodes?.[nodeID] || { state: 'waiting' }
  switch (t.state) {
    case 'trusted':
      return (
        <Badge tone="ok" dot>
          fleet
        </Badge>
      )
    case 'unsupported':
      return (
        <span title={`${t.error} - until then the Controller reaches it with the credential it got when it was added`}>
          <Badge tone="warn">needs an update</Badge>
        </span>
      )
    case 'other-fleet':
      return (
        <span title={t.error}>
          <Badge tone="danger">trusts another fleet</Badge>
        </span>
      )
    case 'error':
      return (
        <span title={t.error}>
          <Badge tone="danger">fleet: {t.error?.slice(0, 40) || 'error'}</Badge>
        </span>
      )
    default:
      return <Badge>joining the fleet…</Badge>
  }
}
