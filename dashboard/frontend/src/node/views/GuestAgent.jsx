import { Bot, ScrollText } from 'lucide-react'
import { Badge, Card, Loading, PageHeader } from '../../shared/ui.jsx'
import { usePoll } from '../hooks.jsx'

const ID = 'qemu-guest-agent'

// GuestAgent is the qemu-guest-agent extension: whether the hypervisor
// talks to it, and what it may do on the node.
export default function GuestAgent() {
  const services = usePoll('/api/system/services', { every: 10000 })
  if (!services.data) return <Loading />
  const svc = (services.data.services || []).find((s) => s.id === ID)
  let badge = <Badge tone="warn">{svc?.state || 'unknown'}</Badge>
  if (svc?.state === 'running') badge = <Badge tone="ok" dot>Running</Badge>
  else if (svc?.state === 'waiting') badge = <Badge tone="warn">Waiting for the hypervisor</Badge>
  return (
    <>
      <PageHeader title="QEMU guest agent" subtitle="qemu-guest-agent · lets Proxmox and other KVM hypervisors see and stop the node" />
      <div className="stack">
        <Card
          title="qemu-ga"
          icon={Bot}
          actions={
            <a href={`#/logs?service=${ID}`} className="small">
              <ScrollText size={12} /> Logs
            </a>
          }
        >
          <div className="spread">
            <span>{svc?.state === 'running' ? 'The hypervisor talks to the node through its virtio-serial channel.' : "The agent waits for the hypervisor's channel."}</span>
            {badge}
          </div>
          {svc?.state === 'waiting' && (
            <div className="notice" style={{ marginTop: '0.75rem' }}>
              The VM has no guest agent channel. In Proxmox, enable <strong>Options › QEMU Guest Agent</strong>, then stop and start the VM - a reboot from inside doesn&apos;t add the device. The agent starts by itself once it&apos;s there.
            </div>
          )}
        </Card>
        <Card title="What the hypervisor can do">
          <ul className="small" style={{ margin: 0 }}>
            <li>Read the node&apos;s addresses, OS, hostname, time and filesystems - what Proxmox shows in the VM summary.</li>
            <li>Freeze and thaw the filesystems around a backup snapshot, for a consistent copy.</li>
            <li>Shut the node down cleanly: HAProxy is soft-stopped first, like the API&apos;s Shutdown.</li>
          </ul>
          <p className="muted small" style={{ marginBottom: 0 }}>
            Running programs, reading or writing files and changing passwords through the agent are disabled - the node stays shell-less.
          </p>
        </Card>
      </div>
    </>
  )
}
