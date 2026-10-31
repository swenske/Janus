import { CheckCircle2, Copy, Download, KeyRound, RefreshCw, ShieldCheck, Upload } from 'lucide-react'
import { useState } from 'react'
import { postJSON } from './call.js'
import { fleetAlert, needsKit, renewalProgress } from './fleetAlert.js'
import { Badge, Card, useAction, useConfirm, useToast } from './shared/ui.jsx'
import { fmtDate } from './shared/units.js'

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

// KitForm takes the recovery kit and its passphrase back - to confirm
// the fleet, renew its issuing CA, or reach the nodes only the kit does.
function KitForm({ busy, action, icon: Icon = Upload, onSubmit, onCancel, cancelLabel = 'Cancel', children }) {
  const [kit, setKit] = useState('')
  const [typed, setTyped] = useState('')
  const readKit = (e) => {
    const f = e.target.files?.[0]
    if (f) f.text().then(setKit)
  }
  const submit = (e) => {
    e.preventDefault()
    onSubmit(kit, typed)
  }
  return (
    <form className="stack" style={{ gap: '0.6rem' }} onSubmit={submit}>
      {children}
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
          <Icon size={15} /> {action}
        </button>
        {onCancel && (
          <button type="button" className="ghost" onClick={onCancel}>
            {cancelLabel}
          </button>
        )}
      </div>
    </form>
  )
}

// FleetSetup is shown until the fleet is ready.
export function FleetSetup({ fleet, onChanged }) {
  const [passphrase, setPassphrase] = useState(null)
  const [kitFile, setKitFile] = useState('')
  const [saved, setSaved] = useState(false)
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
  const prove = (kit, passphrase) =>
    run(async () => {
      await postJSON('/api/fleet/confirm', { kit, passphrase })
      await onChanged()
    }, 'The fleet is ready - its root key is gone from this Controller')

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
            <KitForm busy={busy} action="Confirm" onSubmit={prove} onCancel={() => setSaved(false)} cancelLabel="Back">
              <div className="muted small">
                Give the kit and its passphrase back: once they open, the root key is deleted from this Controller, and your nodes are brought to trust the fleet.
              </div>
            </KitForm>
          </Step>
        </ol>
        <MasterKeyNotice fleet={fleet} />
      </div>
    </Card>
  )
}

const day = (s) => (s ? fmtDate(s) : '–')

// Renewal is the issuing CA's renewal: its notice when due, where a
// renewal stands, and for an admin the kit's form - to renew, or to reach
// the nodes only the kit does.
function Renewal({ fleet, nodes, isAdmin, onChanged }) {
  const [open, setOpen] = useState(null) // 'renew' | 'rescue'
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const alert = fleetAlert(fleet)
  const progress = renewalProgress(fleet)
  const locked = needsKit(fleet)
  const name = (id) => nodes?.find((n) => n.id === id)?.name || id
  const send = (path, done) => (kit, passphrase) =>
    run(async () => {
      await postJSON(`/api/fleet/${path}`, { kit, passphrase })
      setOpen(null)
      await onChanged?.()
    }, done)
  const startRenew = async () => {
    const ok = await confirm({
      title: 'Renew the issuing CA?',
      body: (
        <>
          <p>
            The recovery kit and its passphrase make a new issuing CA, valid 2 years, and a bundle listing it and the current one. The Controller brings every node to that
            bundle - reaching each with a certificate it already lets in -, then signs janusctl&apos;s certificates with the new CA.
          </p>
          <p className="muted">The root&apos;s key leaves this Controller again: only a certificate it signs stays an hour in memory, for nodes no other reaches.</p>
        </>
      ),
      action: 'Continue',
    })
    if (ok) setOpen('renew')
  }
  return (
    <>
      {alert && <div className={`notice ${alert.level} small`}>{alert.text}</div>}
      {progress && (
        <div className="notice small">
          Renewing: bundle version {progress.version} goes to every node -{' '}
          {progress.behind.length === 0 ? 'all have it' : `still on an older one: ${progress.behind.join(', ')}`}. janusctl&apos;s certificates come from the new CA once all
          have it{progress.switchBy ? `, or on ${day(progress.switchBy)} at the latest` : ''}.
        </div>
      )}
      {locked.length > 0 && (
        <div className="notice danger small">
          {locked.length === 1 ? '1 node' : `${locked.length} nodes`} the Controller reaches only with the recovery kit - their bundle lists no issuing CA it can still sign
          with: {locked.map((l) => name(l.id)).join(', ')}.
        </div>
      )}
      {isAdmin && open === null && (
        <div className="row">
          <button className="small" onClick={startRenew} disabled={busy || !!progress}>
            <RefreshCw size={14} /> Renew the issuing CA…
          </button>
          {locked.length > 0 && (
            <button className="small" onClick={() => setOpen('rescue')} disabled={busy}>
              <KeyRound size={14} /> Reach them with the recovery kit…
            </button>
          )}
        </div>
      )}
      {isAdmin && open === 'renew' && (
        <KitForm busy={busy} action="Renew" icon={RefreshCw} onSubmit={send('renew', 'Issuing CA renewed - the nodes are brought to its bundle')} onCancel={() => setOpen(null)}>
          <div className="muted small">The recovery kit and its passphrase - the ones you stored when the fleet was set up.</div>
        </KitForm>
      )}
      {isAdmin && open === 'rescue' && (
        <KitForm busy={busy} action="Reach them" icon={KeyRound} onSubmit={send('rescue', 'The kit opens the way for an hour')} onCancel={() => setOpen(null)}>
          <div className="muted small">For an hour, the Controller reaches them with a certificate the root signs, and brings them to the fleet&apos;s bundle.</div>
        </KitForm>
      )}
    </>
  )
}

// FleetCard is the fleet once ready.
export function FleetCard({ fleet, nodes, isAdmin, onChanged }) {
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
          {fleet.renewal?.renewed && (
            <>
              <dt>Renewed</dt>
              <dd>
                {day(fleet.renewal.renewed)}
                {fleet.renewal.previous_name ? ` - the CA before until ${day(fleet.renewal.previous_not_after)}` : ''}
              </dd>
            </>
          )}
          <dt>Set up</dt>
          <dd>{day(fleet.confirmed)}</dd>
        </dl>
        <Renewal fleet={fleet} nodes={nodes} isAdmin={isAdmin} onChanged={onChanged} />
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
    case 'needs-kit':
      return (
        <span className="trust-error" title={t.error || ''}>
          <Badge tone="danger">needs the recovery kit</Badge>
        </span>
      )
    case 'other-fleet':
      return (
        <span title={t.error}>
          <Badge tone="danger">trusts another fleet</Badge>
        </span>
      )
    case 'error':
      // The error in full below the badge - wrapped anywhere (a URL, a
      // certificate's chain), three lines at most, all of it in the
      // tooltip: a badge's text doesn't wrap and ran out of the card.
      return (
        <span className="trust-error" title={t.error || ''}>
          <Badge tone="danger">fleet error</Badge>
          {t.error && <span className="trust-error-text">{t.error}</span>}
        </span>
      )
    default:
      return <Badge>joining the fleet…</Badge>
  }
}
