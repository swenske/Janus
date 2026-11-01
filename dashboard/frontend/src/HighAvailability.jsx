import { ArrowLeftRight, Copy, Plus, ServerCog, Trash2, X } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { call, postJSON } from './call.js'
import { fmtDateTime } from './shared/units.js'
import { Badge, Card, ErrorBox, useAction, useConfirm, useToast } from './shared/ui.jsx'

// HighAvailability is the Settings card of the warm standby (/api/ha,
// admins; docs/guide/controller-standby.md): pairing a standby with a
// token shown once, what the standby last copied, the switchover to it,
// and forgetting it.
export default function HighAvailability() {
  const [view, setView] = useState(null)
  const [error, setError] = useState(null)
  const [token, setToken] = useState(null)
  const [switching, setSwitching] = useState(false)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const load = useCallback((background) => call('/api/ha', { background }).then(setView, setError), [])
  useEffect(() => {
    load()
    const t = setInterval(() => load(true), 30000)
    return () => clearInterval(t)
  }, [load])
  const pair = () =>
    run(async () => {
      setToken(await postJSON('/api/ha/pairing'))
    })
  const remove = async () => {
    const ok = await confirm({
      title: `Remove the standby ${view.standby.name}?`,
      body: (
        <p>
          It can&apos;t copy this Controller any more: its last copy grows old. Stop it on its host too - and pair it again from a new data directory to keep a standby.
        </p>
      ),
      action: 'Remove',
      danger: true,
    })
    if (!ok) return
    await run(async () => {
      await postJSON('/api/ha/standby', undefined, 'DELETE')
      setToken(null)
      await load()
    }, 'The standby is removed')
  }
  const switchover = async () => {
    const ok = await confirm({
      title: `Switch over to ${view.standby.name}?`,
      body: (
        <>
          <p>
            This Controller hands {view.standby.name} a last copy and starts again as its standby; {view.standby.name} starts as the active, epoch {view.epoch + 1}.
          </p>
          <p>
            Everyone&apos;s session ends - sign in again on the active. Behind a VIP or a DNS name both serve, this page comes back on {view.standby.name} in a few
            seconds; otherwise go to its own address.
          </p>
        </>
      ),
      action: 'Switch over',
      danger: true,
      typeToConfirm: view.standby.name,
    })
    if (!ok) return
    const done = await run(async () => {
      await postJSON('/api/ha/switchover')
      return true
    }, 'The standby is taking over')
    if (done) setSwitching(true)
  }
  const sb = view?.standby
  const peer = view?.peer
  return (
    <Card
      title="High availability"
      icon={ServerCog}
      actions={view && (sb ? <Badge tone="ok">standby {sb.name}</Badge> : <Badge>no standby</Badge>)}
    >
      <p className="muted small" style={{ marginTop: 0 }}>
        A warm standby: a second Controller that copies this one every minute and takes over when promoted - by hand, never by itself.{' '}
        <a href="https://janus.sw-servers.net/docs/guide/controller-standby/">How it works</a>.
      </p>
      {error && <ErrorBox error={error} />}
      {switching && (
        <div className="notice warn">
          The standby is taking over: this Controller stops answering in a moment, then starts again as its standby. Sign in again on the active.
        </div>
      )}
      {view && !switching && (
        <div className="stack">
          {sb ? (
            <>
              <dl className="kv">
                <dt>Standby</dt>
                <dd>
                  {sb.name} <span className="mono small muted">{sb.address}</span>
                </dd>
                <dt>Paired</dt>
                <dd>{fmtDateTime(sb.paired_at)}</dd>
                <dt>Last copy</dt>
                <dd>
                  {view.last_copy ? (
                    <>
                      {ago(view.last_copy.at)}
                      {view.last_copy.version && <span className="muted small"> - the standby runs {view.last_copy.version}</span>}
                    </>
                  ) : (
                    <span className="muted">none yet</span>
                  )}
                </dd>
                <dt>The standby</dt>
                <dd>
                  {!peer ? (
                    <span className="muted">not asked yet</span>
                  ) : peer.error ? (
                    <span className="small">
                      <Badge tone="danger">no answer</Badge> {peer.error} <span className="muted">({ago(peer.checked)})</span>
                    </span>
                  ) : (
                    <span>
                      {peer.role}, epoch {peer.epoch}
                      {peer.version && <span className="muted small"> - {peer.version}</span>}
                      <span className="muted small"> ({ago(peer.checked)})</span>
                    </span>
                  )}
                </dd>
                <dt>Epoch</dt>
                <dd>{view.epoch}</dd>
              </dl>
              {view.last_copy && staleCopy(view.last_copy.at) && (
                <div className="notice warn small">The standby hasn&apos;t copied this Controller for more than 5 minutes: is it running? Its page says why.</div>
              )}
              <div className="row">
                <button className="small" disabled={busy} onClick={switchover}>
                  <ArrowLeftRight size={13} /> Switch over…
                </button>
                <button className="ghost small danger" disabled={busy} onClick={remove}>
                  <Trash2 size={13} /> Remove the standby…
                </button>
              </div>
            </>
          ) : token ? (
            <PairingToken token={token} onClose={() => setToken(null)} />
          ) : (
            <div>
              <button className="small" disabled={busy} onClick={pair}>
                <Plus size={13} /> Add a standby
              </button>
            </div>
          )}
        </div>
      )}
    </Card>
  )
}

// PairingToken shows a pairing token once, with the command the standby's
// host runs.
function PairingToken({ token, onClose }) {
  const toast = useToast()
  const peer = token.register_address || 'THIS-CONTROLLER:8443'
  const command = `dashboardd standby -data-dir /data -peer ${peer} -pair ${token.token}`
  const copy = async (text, what) => {
    try {
      await navigator.clipboard.writeText(text)
      toast(`${what} copied`)
    } catch {
      toast(`Select the ${what.toLowerCase()} and press Ctrl+C`, 'warn')
    }
  }
  return (
    <div className="notice">
      <div className="spread">
        <strong>The pairing token - copy it now, it won&apos;t be shown again</strong>
        <button className="ghost icon" onClick={onClose} title="Close">
          <X size={15} />
        </button>
      </div>
      <div className="row" style={{ margin: '0.5rem 0' }}>
        <input className="mono grow" readOnly value={token.token} onFocus={(e) => e.target.select()} aria-label="The pairing token" />
        <button className="small" onClick={() => copy(token.token, 'Token')}>
          <Copy size={13} /> Copy
        </button>
      </div>
      <div className="small">
        On the standby&apos;s host, on a new data directory - the same Controller image -, then start the Controller there: it runs as the standby.
      </div>
      <div className="row" style={{ margin: '0.5rem 0' }}>
        <input className="mono grow" readOnly value={command} onFocus={(e) => e.target.select()} aria-label="The standby's command" />
        <button className="small" onClick={() => copy(command, 'Command')}>
          <Copy size={13} /> Copy
        </button>
      </div>
      <div className="small muted">
        <code>-peer</code> is this Controller&apos;s own registration address - not a VIP that moves with the active; add <code>-address</code> with the standby&apos;s
        own when this Controller reaches it by another. Good once, until {fmtDateTime(token.expires)}.
      </div>
    </div>
  )
}

function ago(t) {
  if (!t || t.startsWith('0001')) return 'never'
  const s = Math.max(0, Math.round((Date.now() - new Date(t).getTime()) / 1000))
  return s < 60 ? `${s} s ago` : s < 3600 ? `${Math.round(s / 60)} min ago` : fmtDateTime(t)
}

function staleCopy(t) {
  return t && Date.now() - new Date(t).getTime() > 5 * 60 * 1000
}
