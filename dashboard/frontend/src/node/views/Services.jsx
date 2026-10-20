import { useState } from 'react'
import { Play, RotateCw, Server, Square, Terminal } from 'lucide-react'
import { postJSON } from '../api.js'
import { Badge, Card, ErrorBox, PageHeader, stateTone, useAction, useConfirm } from '../../shared/ui.jsx'
import { usePoll } from '../hooks.jsx'
import { useMay } from '../may.js'

const ABOUT = {
  janusd: 'The node control plane: this API, PKI, lifecycle. Restarting it does not interrupt HAProxy.',
  haproxy: 'The load balancer. Restart is seamless: a new process takes over the listening sockets and the old one finishes its connections.',
}

export default function Services() {
  const { data, error, reload } = usePoll('/api/system/services')
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const may = useMay()
  const act = async (id, action) => {
    if (action === 'stop') {
      const ok = await confirm({ title: `Stop ${id}?`, body: <p>It finishes in-flight connections (up to 10 s) and then stops serving traffic until started again.</p>, action: 'Stop', danger: true })
      if (!ok) return
    }
    await run(() => postJSON(`/api/system/services/${id}/${action}`), `${id}: ${action} requested`)
    setTimeout(reload, 800)
  }
  const modules = usePoll('/api/network/modules', { every: 0 })
  return (
    <>
      <PageHeader title="Services" subtitle="Processes this node manages" />
      <ErrorBox error={error} />
      <div className="grid grid-2">
        {(data?.services || []).map((s) => (
          <Card
            key={s.id}
            title={<span className="mono">{s.id}</span>}
            icon={Server}
            actions={
              <>
                <Badge tone={stateTone(s.state)} dot>
                  {s.state}
                </Badge>
                <Badge tone={stateTone(s.health)}>{s.health}</Badge>
              </>
            }
          >
            <p className="muted" style={{ marginTop: 0 }}>
              {ABOUT[s.id]}
            </p>
            <div className="row" hidden={!may('SystemService/ServiceRestart')}>
              {s.id === 'haproxy' && s.state !== 'running' && (
                <button className="primary" disabled={busy} onClick={() => act(s.id, 'start')}>
                  <Play size={15} /> Start
                </button>
              )}
              <button disabled={busy} onClick={() => act(s.id, 'restart')}>
                <RotateCw size={15} /> Restart
              </button>
              {s.id === 'haproxy' && s.state === 'running' && (
                <button className="danger" disabled={busy} onClick={() => act(s.id, 'stop')}>
                  <Square size={15} /> Stop
                </button>
              )}
              {s.id === 'janusd' && <span className="muted small">Can't be stopped - the node would be unreachable.</span>}
            </div>
          </Card>
        ))}
        <ConsoleCard may={may} />
        {['bgp', 'vrrp', 'firewall'].map((m) => {
          const names = { bgp: 'bird (BGP)', vrrp: 'keepalived (VRRP)', firewall: 'nftables (firewall)' }
          const st = modules.data?.[m]?.state
          return (
            <Card key={m} title={<span className="mono">{names[m]}</span>} icon={Server} actions={<Badge tone={stateTone(st)}>{st === 'not_enabled' ? 'not in this image' : st || '…'}</Badge>}>
              <p className="muted" style={{ margin: 0 }}>
                Optional module - <a href={`#/apps/${m}`}>details</a>.
              </p>
            </Card>
          )
        })}
      </div>
    </>
  )
}

// ConsoleCard: the services' share of the node's console - a few lines a
// second of what they print, the rest kept in their logs. An emulated
// serial port costs CPU for every byte (init's): a node that logs every
// request may want less, or none of it.
function ConsoleCard({ may }) {
  const { data, error, reload } = usePoll('/api/system/console-budget', { every: 0 })
  const [busy, run] = useAction()
  const [draft, setDraft] = useState(null)
  const canSet = may('SystemService/ConsoleBudgetSet')
  const cfg = data?.config
  const rate = draft ?? String(cfg?.rate_bytes_per_second ?? '')
  const rateValid = /^\d+$/.test(rate)
  const changed = cfg && rateValid && Number(rate) !== (cfg.rate_bytes_per_second || 0)
  const set = (body, done) =>
    run(async () => {
      await postJSON('/api/system/console-budget', body)
      setDraft(null)
      reload()
    }, done)
  const badge = !cfg ? <Badge>…</Badge> : cfg.rate_bytes_per_second === 0 ? <Badge>Services' output off</Badge> : data.is_default ? <Badge tone="ok">Default</Badge> : <Badge tone="ok">{cfg.rate_bytes_per_second} B/s</Badge>
  return (
    <Card title="Console" icon={Terminal} actions={badge}>
      <ErrorBox error={error} />
      <p className="muted" style={{ marginTop: 0 }}>
        What the node's serial console gets of the services' output, bytes a second - the rest stays in their logs. An emulated serial port costs CPU for every byte: a node that logs every request may want less, or none (janusd's own log always shows).
      </p>
      {data && (
        <p className="small muted" style={{ marginTop: 0 }}>
          Lines left out since janusd started: {data.lines_left_out_total ?? 0}. Default: {data.default?.rate_bytes_per_second} B/s, {data.default?.burst_bytes} B at once.
        </p>
      )}
      <div className="row" hidden={!canSet}>
        <label className="small">
          Bytes a second{' '}
          <input className="mono" style={{ width: '7em' }} value={rate} onChange={(e) => setDraft(e.target.value)} disabled={busy || !cfg} />
        </label>
        <button className="primary" disabled={busy || !changed} onClick={() => set({ rate_bytes_per_second: Number(rate), burst_bytes: Math.max(Number(rate), cfg?.burst_bytes || 0) }, Number(rate) === 0 ? 'Console: the services\' output turned off' : `Console budget: ${rate} B/s`)}>
          Apply
        </button>
        <button disabled={busy || !cfg || cfg.rate_bytes_per_second === 0} onClick={() => set({ rate_bytes_per_second: 0, burst_bytes: 0 }, 'Console: the services\' output turned off')}>
          Turn off
        </button>
        <button disabled={busy || !data || data.is_default} onClick={() => set({ use_default: true }, 'Console budget: the default again')}>
          Default
        </button>
      </div>
    </Card>
  )
}
