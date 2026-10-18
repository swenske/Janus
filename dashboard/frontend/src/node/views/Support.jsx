import { CircleCheck, CircleMinus, Download, FileArchive, ShieldCheck } from 'lucide-react'
import { download } from '../api.js'
import { bytes } from '../format.js'
import { Card, PageHeader, useAction } from '../../shared/ui.jsx'
import { useMay } from '../may.js'

// What the bundle holds, by the call each part needs - internal/support
// leaves out what the account may not read.
const PARTS = [
  { needs: 'SystemService/Version', what: 'Version and image, services, processes, mounts, memory, CPU, disks' },
  { needs: 'NetworkService/NetworkStatus', what: 'Network state and configuration, sockets, the firewall, VRRP, BGP and Consul states' },
  { needs: 'SystemService/SysctlList', what: 'Kernel parameters and their history' },
  { needs: 'HAProxyService/ShowInfo', what: 'HAProxy’s state, statistics and backends' },
  { needs: 'HAProxyService/GetConfig', what: 'haproxy.cfg - its passwords, credential headers and setenv values masked' },
  { needs: 'SystemService/Events', what: 'The event log' },
  { needs: 'SystemService/Logs', what: 'Each service’s logs, this boot and the one before' },
  { needs: 'SystemService/Dmesg', what: 'The kernel’s messages, and the SELinux denials among them' },
  { needs: 'SystemService/Audit', what: 'The API’s audit log: who called what, from where' },
]

export default function Support() {
  const may = useMay()
  const [busy, run] = useAction()
  const save = () => run(() => download('/api/support', 'janus-support.tar.gz'), (r) => `Saved ${r.name} (${bytes(r.size)})`)
  return (
    <>
      <PageHeader title="Diagnostic bundle" subtitle="What to attach to an issue: the node’s state and logs, without its secrets" />
      <div className="grid grid-2">
        <Card title="What it holds for you" icon={FileArchive}>
          <div className="stack">
            {PARTS.map((p) => (
              <div key={p.needs} className="row" style={{ flexWrap: 'nowrap', alignItems: 'flex-start' }}>
                {may(p.needs) ? <CircleCheck size={15} color="var(--ok)" /> : <CircleMinus size={15} color="var(--muted)" />}
                <span className={may(p.needs) ? '' : 'muted'}>
                  {p.what}
                  {!may(p.needs) && ' - not for your role'}
                </span>
              </div>
            ))}
            <div className="row">
              <button className="primary" disabled={busy} onClick={save}>
                <Download size={15} /> {busy ? 'Gathering…' : 'Download'}
              </button>
              <span className="muted small">A tar.gz, read with your permissions; MANIFEST.json in it says what was left out and why.</span>
            </div>
          </div>
        </Card>
        <Card title="Never in it" icon={ShieldCheck}>
          <ul className="small" style={{ margin: 0, paddingLeft: '1.1rem' }}>
            <li>The node’s credentials, and any PEM private key - cut out of every file.</li>
            <li>The configurations whose passwords the node doesn’t mask: BGP, VRRP, Consul.</li>
          </ul>
          <p className="muted small">
            Logs are as the node wrote them: a URL with a token in its query, a client’s address are in them. Read them before sharing the bundle.
          </p>
        </Card>
      </div>
    </>
  )
}
