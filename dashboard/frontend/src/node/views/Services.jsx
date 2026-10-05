import { Play, RotateCw, Server, Square } from 'lucide-react'
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
