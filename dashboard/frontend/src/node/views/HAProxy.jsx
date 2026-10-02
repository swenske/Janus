import { FileCode2, FileText, KeyRound, ListTree, Play, RefreshCw, RotateCw, Server, Shuffle, Square } from 'lucide-react'
import { postJSON } from '../api.js'
import { Badge, PageHeader, Tabs, stateTone, useAction, useConfirm } from '../../shared/ui.jsx'
import { navigate, useMetrics, usePoll } from '../hooks.jsx'
import Backends from './haproxy/Backends.jsx'
import Certificates from './haproxy/Certificates.jsx'
import Config from './haproxy/Config.jsx'
import Files from './haproxy/Files.jsx'
import MapsAcls from './haproxy/MapsAcls.jsx'
import Status from './haproxy/Status.jsx'

const TABS = [
  { id: 'status', label: 'Status', icon: Shuffle, view: Status },
  { id: 'backends', label: 'Backends', icon: Server, view: Backends },
  { id: 'config', label: 'Configuration', icon: FileCode2, view: Config },
  { id: 'maps', label: 'Maps & ACLs', icon: ListTree, view: MapsAcls },
  { id: 'certificates', label: 'Certificates', icon: KeyRound, view: Certificates },
  { id: 'files', label: 'Files', icon: FileText, view: Files },
]

function ServiceControls() {
  const services = usePoll('/api/system/services')
  const { refresh } = useMetrics()
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const hap = services.data?.services?.find((s) => s.id === 'haproxy')
  const act = async (action, label) => {
    if (action === 'stop') {
      const ok = await confirm({
        title: 'Stop HAProxy?',
        body: <p>HAProxy stops accepting connections and finishes the ones in flight (up to 10 s). This node won't serve any traffic until you start it again.</p>,
        action: 'Stop HAProxy',
        danger: true,
      })
      if (!ok) return
    }
    await run(() => postJSON(`/api/system/services/haproxy/${action}`), label)
    services.reload()
    refresh()
  }
  return (
    <div className="row">
      {hap && (
        <>
          <Badge tone={stateTone(hap.state)} dot>
            {hap.state}
          </Badge>
          <Badge tone={stateTone(hap.health)}>{hap.health}</Badge>
        </>
      )}
      <button
        disabled={busy}
        onClick={() => run(() => postJSON('/api/haproxy/reload'), 'HAProxy reloaded seamlessly').then(() => services.reload())}
        title="Re-read the current configuration without dropping connections"
      >
        <RefreshCw size={15} /> Reload
      </button>
      {hap?.state === 'running' ? (
        <>
          <button disabled={busy} onClick={() => act('restart', 'HAProxy restarted seamlessly')} title="New process takes over the sockets; the old one finishes its connections">
            <RotateCw size={15} /> Restart
          </button>
          <button className="danger" disabled={busy} onClick={() => act('stop', 'HAProxy stopped')}>
            <Square size={15} /> Stop
          </button>
        </>
      ) : (
        <button className="primary" disabled={busy} onClick={() => act('start', 'HAProxy started')}>
          <Play size={15} /> Start
        </button>
      )}
    </div>
  )
}

export default function HAProxy({ route }) {
  const sub = route.split('/')[2] || 'status'
  const tab = TABS.find((t) => t.id === sub) || TABS[0]
  const View = tab.view
  return (
    <>
      <PageHeader title="HAProxy" subtitle="The load balancer this node runs" actions={<ServiceControls />} />
      <Tabs tabs={TABS} active={tab.id} onChange={(id) => navigate(id === 'status' ? '/haproxy' : `/haproxy/${id}`)} />
      <View />
    </>
  )
}

