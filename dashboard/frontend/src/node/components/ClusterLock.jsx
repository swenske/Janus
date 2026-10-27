import { ExternalLink, Lock, Network, Unlock } from 'lucide-react'
import { postJSON } from '../api.js'
import { DOMAIN_WORDS, bgpState, clusterLink, routerRoles, vrrpRole } from '../cluster.js'
import { useCluster } from '../clusterContext.js'
import { usePoll } from '../hooks.jsx'
import { useMay } from '../may.js'
import { Badge, useAction, useConfirm } from '../../shared/ui.jsx'

// ClusterBadge, in the topbar: the cluster this node is a member of - a
// link to its page on the Controller -, its VRRP role when the cluster
// manages VRRP, its BGP state when it manages BGP - what the node knows,
// always; the router's choice too when the cluster's RouterOS hook reads
// it -, and drained when it is out of service.
export function ClusterBadge() {
  const { cluster } = useCluster()
  const vrrp = usePoll('/api/network/vrrp', { every: 15000, enabled: !!cluster?.managed?.includes('vrrp') })
  const managesBGP = !!cluster?.managed?.includes('bgp')
  const bgpStatus = usePoll('/api/network/bgp', { every: 15000, enabled: managesBGP })
  const router = usePoll('/api/cluster/router', { every: 15000, enabled: managesBGP })
  if (!cluster) return null
  const role = vrrpRole(vrrp.data)
  const bgp = managesBGP ? bgpState(bgpStatus.data) : { state: '' }
  const routes = managesBGP ? routerRoles(router.data) : { label: '' }
  const link = clusterLink(cluster)
  const title = `Member ${cluster.member_name} of cluster ${cluster.name}${cluster.managed?.length ? ` - it manages ${cluster.managed.join(', ')} here` : ''}${link ? ' - open the cluster' : ' (janusctl cluster)'}`
  const badge = (
    <Badge tone="accent">
      <Network size={12} /> {cluster.name} · {cluster.member_name}
    </Badge>
  )
  return (
    <span className="cluster-badge" title={title}>
      {link ? (
        <a href={link} className="version-link">
          {badge}
        </a>
      ) : (
        badge
      )}
      {role && (
        <Badge tone={role === 'MASTER' ? 'ok' : role === 'FAULT' ? 'danger' : ''} dot>
          {role === 'MASTER' ? 'VRRP master' : `VRRP ${role.toLowerCase()}`}
        </Badge>
      )}
      {bgp.state && (
        <span title={bgpTitle(bgp)}>
          <Badge tone={bgp.state === 'announcing' ? (bgp.partial ? 'warn' : 'ok') : bgp.state === 'withdrawn' ? 'warn' : 'danger'} dot>
            BGP {bgp.state}
          </Badge>
        </span>
      )}
      {routes.label && (
        <span title={routerTitle(router.data, routes)}>
          <Badge tone={routes.roles.every((r) => r.role === 'multipath' || r.role === 'master') ? 'ok' : routes.roles.some((r) => r.role === 'absent') ? 'danger' : ''}>
            router: {routes.label}
          </Badge>
        </span>
      )}
      {cluster.drained && <Badge tone="warn">drained</Badge>}
    </span>
  )
}

// bgpTitle is the BGP badge's tooltip: each session, and why nothing is
// announced.
function bgpTitle(bgp) {
  const sessions = bgp.sessions.map((s) => `${s.name}: ${s.established ? `established, ${s.exported} route(s) announced` : 'not established'}`).join('; ')
  const why =
    bgp.state === 'withdrawn' ? ' - its haproxy_* routes are held down: HAProxy not answering, or the member drained' : bgp.state === 'down' ? ' - no session with the router' : ''
  return `What this node announces over BGP: ${sessions}${why}. Whether the router uses it is the router's to say.`
}

// routerTitle is the router badge's tooltip: each prefix's routes, read by
// the cluster's RouterOS hook.
function routerTitle(view, routes) {
  const each = routes.roles
    .map((r) => {
      const of = `${r.active} of ${r.total} route(s) to it active`
      if (r.role === 'absent') return `${r.prefix}: absent - no route through this member (${of})`
      if (r.role === 'backup') return `${r.prefix}: backup - its route present, not used (${of})`
      return `${r.prefix}: ${r.role}, ${of}`
    })
    .join('; ')
  const at = view?.read_at ? ` (read ${new Date(view.read_at).toLocaleTimeString()})` : ''
  return `The router's routes through this member, as the cluster's RouterOS hook reads them${at}: ${each}.`
}

// ClusterLockNotice says, on a page that changes a domain, that the
// node's cluster manages it here: the node refuses a change made from
// this page (useMay doesn't offer it), the cluster's page is where to make
// it. An admin may release this node's lock for an emergency change.
export function ClusterLockNotice({ domain }) {
  const { cluster, reload } = useCluster()
  const may = useMay()
  const confirm = useConfirm()
  const [busy, run] = useAction()
  if (!cluster?.managed?.includes(domain)) return null
  const words = DOMAIN_WORDS[domain] || domain
  const link = clusterLink(cluster)
  const release = async () => {
    const ok = await confirm({
      title: `Release ${words} on ${cluster.member_name}?`,
      body: (
        <>
          <p>
            This node then accepts changes to {words} made here, for an emergency. Cluster {cluster.name} reports it as drifted - and never overwrites it by itself -
            until its next apply takes {words} back and locks it again.
          </p>
          <p>The other members keep running the cluster&apos;s version.</p>
        </>
      ),
      action: 'Release',
      danger: true,
    })
    if (!ok) return
    await run(() => postJSON('/api/cluster/release', { domains: [domain] }), `${words} released on ${cluster.member_name}`)
    reload()
  }
  return (
    <div className="notice cluster-lock">
      <Lock size={15} />
      <div className="grow">
        <strong>
          {words.charAt(0).toUpperCase() + words.slice(1)} is managed by cluster {cluster.name}
        </strong>
        : change it on the cluster, every member alike - this node refuses a change made here.{' '}
        {link && (
          <a href={link}>
            Open cluster {cluster.name} <ExternalLink size={12} />
          </a>
        )}
      </div>
      {may('ClusterService/ClusterRelease') && (
        <button className="small" disabled={busy} onClick={release} title="An emergency change on this node only: the cluster reports it as drifted">
          <Unlock size={14} /> Release this node&apos;s lock…
        </button>
      )}
    </div>
  )
}
