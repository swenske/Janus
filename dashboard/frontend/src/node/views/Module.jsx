import { Boxes, Shield, Workflow } from 'lucide-react'
import { Badge, Card, Loading, PageHeader, stateTone } from '../../shared/ui.jsx'
import { usePoll } from '../hooks.jsx'

const MODULES = {
  bgp: {
    title: 'BGP',
    daemon: 'bird',
    icon: Workflow,
    about: 'Announce this node\'s virtual IPs to your routers over BGP, e.g. for anycast or ECMP load balancing across several Janus nodes.',
  },
  vrrp: {
    title: 'VRRP',
    daemon: 'keepalived',
    icon: Boxes,
    about: 'Share a virtual IP between two or more Janus nodes, with automatic failover when the active one goes down.',
  },
  firewall: {
    title: 'Firewall',
    daemon: 'nftables',
    icon: Shield,
    about: 'Filter traffic on the node itself with an nftables ruleset managed through the API.',
  },
}

export default function Module({ module }) {
  const m = MODULES[module]
  const { data, loading } = usePoll('/api/network/modules', { every: 0 })
  const state = data?.[module]
  return (
    <>
      <PageHeader title={`${m.title} · ${m.daemon}`} subtitle="Optional network module" />
      <Card
        title={m.daemon}
        icon={m.icon}
        actions={state && <Badge tone={stateTone(state.state)}>{state.state === 'not_enabled' ? 'not in this image' : state.state}</Badge>}
      >
        <p style={{ marginTop: 0 }}>{m.about}</p>
        {loading && !data ? (
          <Loading />
        ) : state?.state === 'not_enabled' ? (
          <div className="notice">
            <strong>{m.daemon} isn't part of this node's image.</strong> Janus keeps optional modules out of the image entirely unless they're chosen when the image is
            built - a node without BGP doesn't just have bird disabled, it doesn't have the binary at all. Management from this page arrives with the images that ship
            it.
          </div>
        ) : state?.state === 'error' ? (
          <div className="error-box">{state.message}</div>
        ) : (
          <div className="muted">State: {state?.state}</div>
        )}
      </Card>
    </>
  )
}
