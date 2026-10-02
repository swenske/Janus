import {
  Activity,
  AlertTriangle,
  Archive,
  BarChart3,
  Bot,
  Boxes,
  Cable,
  ChartLine,
  CirclePlus,
  Cpu,
  FolderOpen,
  Gauge,
  HardDrive,
  KeyRound,
  LayoutDashboard,
  LockKeyhole,
  Menu,
  Network,
  Power,
  Radar,
  RefreshCw,
  ScrollText,
  Server,
  Shield,
  Shuffle,
  Terminal,
  Waypoints,
  Workflow,
} from 'lucide-react'
import { useEffect, useState } from 'react'
import { Logo, ThemeToggle } from '../shared/theme.jsx'
import { Badge } from '../shared/ui.jsx'
import { INTERVALS, navigate, useHashRoute, useMetrics, usePoll, useRefresh } from './hooks.jsx'
import Access from './views/Access.jsx'
import Capture from './views/Capture.jsx'
import Events from './views/Events.jsx'
import Files from './views/Files.jsx'
import HAProxy from './views/HAProxy.jsx'
import Kernel from './views/Kernel.jsx'
import Logs from './views/Logs.jsx'
import Metrics from './views/Metrics.jsx'
import Firewall from './views/Firewall.jsx'
import GuestAgent from './views/GuestAgent.jsx'
import JanusExporter from './views/JanusExporter.jsx'
import NodeExporter from './views/NodeExporter.jsx'
import VRRP from './views/VRRP.jsx'
import BGP from './views/BGP.jsx'
import Consul from './views/Consul.jsx'
import LetsEncrypt from './views/LetsEncrypt.jsx'
import NetworkView from './views/Network.jsx'
import NetworkConfig from './views/NetworkConfig.jsx'
import Overview from './views/Overview.jsx'
import PowerView from './views/Power.jsx'
import Processes from './views/Processes.jsx'
import Services from './views/Services.jsx'
import Storage from './views/Storage.jsx'
import Update from './views/Update.jsx'

const NAV = [
  {
    group: 'Monitoring',
    items: [
      { path: '/', label: 'Overview', icon: LayoutDashboard, view: Overview },
      { path: '/metrics', label: 'Metrics', icon: BarChart3, view: Metrics },
      { path: '/processes', label: 'Processes', icon: Cpu, view: Processes },
      { path: '/network', label: 'Network', icon: Network, view: NetworkView },
      { path: '/storage', label: 'Storage', icon: HardDrive, view: Storage },
    ],
  },
  {
    group: 'Logs',
    items: [
      { path: '/logs', label: 'Service logs', icon: ScrollText, view: Logs },
      { path: '/events', label: 'Events', icon: Activity, view: Events },
      { path: '/kernel', label: 'Kernel (dmesg)', icon: Terminal, view: Kernel },
    ],
  },
  {
    group: 'Apps',
    items: [
      { path: '/haproxy', label: 'HAProxy', icon: Shuffle, view: HAProxy, prefix: true },
      { path: '/apps/janus-exporter', label: 'Janus exporter', icon: Gauge, view: JanusExporter },
      // An extension's app shows when the node's image has the extension
      // (node-exporter: its name before v2026.10.02), a module's when its
      // daemon is in the image.
      { path: '/apps/node-exporter', label: 'Node exporter', icon: ChartLine, view: NodeExporter, extension: ['prometheus-node-exporter', 'node-exporter'] },
      { path: '/apps/guest-agent', label: 'QEMU guest agent', icon: Bot, view: GuestAgent, extension: ['qemu-guest-agent'] },
      { path: '/apps/bgp', label: 'BGP · bird', icon: Workflow, view: BGP, moduleKey: 'bgp' },
      { path: '/apps/vrrp', label: 'VRRP · keepalived', icon: Boxes, view: VRRP, moduleKey: 'vrrp' },
      { path: '/apps/firewall', label: 'Firewall · nftables', icon: Shield, view: Firewall, moduleKey: 'firewall' },
      { path: '/apps/letsencrypt', label: "Let's Encrypt", icon: LockKeyhole, view: LetsEncrypt, extension: ['letsencrypt'] },
      { path: '/apps/consul', label: 'Consul', icon: Waypoints, view: Consul, extension: ['consul'] },
      { path: '/system/update?extensions', label: 'Add or remove apps…', icon: CirclePlus, link: true },
    ],
  },
  {
    group: 'Tools',
    items: [
      { path: '/tools/capture', label: 'Packet capture', icon: Radar, view: Capture },
      { path: '/tools/files', label: 'Files', icon: FolderOpen, view: Files },
    ],
  },
  {
    group: 'System',
    items: [
      { path: '/system/network', label: 'Network', icon: Cable, view: NetworkConfig },
      { path: '/system/services', label: 'Services', icon: Server, view: Services },
      { path: '/system/update', label: 'Update', icon: Archive, view: Update },
      { path: '/system/access', label: 'Access', icon: KeyRound, view: Access },
      { path: '/system/power', label: 'Power', icon: Power, view: PowerView },
    ],
  },
]

function findRoute(full) {
  const route = full.split('?')[0]
  for (const g of NAV) {
    for (const item of g.items) {
      if (item.path === route || (item.prefix && route.startsWith(item.path + '/'))) return item
    }
  }
  return NAV[0].items[0]
}

function RefreshSelect() {
  const { interval, setInterval } = useRefresh()
  const { refresh } = useMetrics()
  return (
    <div className="refresh">
      <button className="ghost icon" title="Refresh now" onClick={refresh}>
        <RefreshCw size={16} />
      </button>
      <select value={interval} onChange={(e) => setInterval(Number(e.target.value))} title="Auto-refresh interval">
        {INTERVALS.map((i) => (
          <option key={i.ms} value={i.ms}>
            {i.ms ? `Every ${i.label}` : 'Auto-refresh off'}
          </option>
        ))}
      </select>
    </div>
  )
}

export default function App() {
  const route = useHashRoute()
  const current = findRoute(route)
  const [navOpen, setNavOpen] = useState(false)
  const { latest, error } = useMetrics()
  const node = usePoll('/api/node', { every: 0 })
  const modules = usePoll('/api/network/modules', { every: 0 })
  const check = usePoll('/api/update-check', { every: 120000 })
  const overview = usePoll('/api/system/overview', { every: 60000 })

  useEffect(() => setNavOpen(false), [route])
  const name = node.data?.name || 'Janus node'
  useEffect(() => {
    document.title = `${name} · Janus`
  }, [name])

  const version = overview.data?.version?.version
  const extensions = (overview.data?.version?.extensions || []).map((e) => e.name)
  // Apps the node doesn't have aren't listed: they'd only say "n/a".
  const shown = (item) => {
    if (item.moduleKey) return !!modules.data && modules.data[item.moduleKey]?.state !== 'not_enabled'
    if (item.extension) return item.extension.some((n) => extensions.includes(n))
    return true
  }
  const latestTag = check.data?.latest
  const updateAvailable = check.data?.state === 'ready' && check.data?.update_available
  const hap = latest?.hap
  const View = current.view

  return (
    <div className={`layout ${navOpen ? 'nav-open' : ''}`}>
      <aside className="sidebar">
        <div className="sidebar-brand">
          <Logo size={26} />
          <div>
            <div className="brand-name">Janus</div>
            <div className="muted small">Controller{node.data?.controller_version ? <span className="mono"> {node.data.controller_version}</span> : null}</div>
          </div>
        </div>
        <nav>
          {NAV.map((g) => (
            <div key={g.group} className="nav-group">
              <div className="nav-group-title">{g.group}</div>
              {g.items.filter(shown).map((item) => {
                const active = item === current
                return (
                  <a key={item.path} href={`#${item.path}`} className={`nav-item ${active ? 'active' : ''} ${item.link ? 'nav-link' : ''}`}>
                    <item.icon size={16} />
                    <span className="grow">{item.label}</span>
                    {item.path === '/system/update' && updateAvailable && <span className="nav-dot" title="Update available" />}
                  </a>
                )
              })}
            </div>
          ))}
        </nav>
      </aside>
      <div className="main">
        <header className="topbar">
          <button className="ghost icon nav-toggle" aria-label="Menu" onClick={() => setNavOpen(!navOpen)}>
            <Menu size={18} />
          </button>
          <div className="node-id">
            <div className="node-name">{name}</div>
            <div className="muted small mono">
              {overview.data?.hostname || '…'} · {node.data?.address || ''}
            </div>
          </div>
          <div className="topbar-status">
            <Badge tone={error ? 'danger' : 'ok'} dot>
              {error ? 'unreachable' : 'janusd'}
            </Badge>
            {hap && (
              <Badge tone={hap.up ? 'ok' : 'danger'} dot>
                HAProxy
              </Badge>
            )}
            {version && (
              <a href="#/system/update" className="version-link" title={updateAvailable ? `Update available: ${latestTag}` : 'Up to date'}>
                <Badge tone={updateAvailable ? 'accent' : ''}>{version}</Badge>
                {updateAvailable && <Badge tone="accent">update → {latestTag}</Badge>}
              </a>
            )}
          </div>
          <span className="grow" />
          <RefreshSelect />
          <ThemeToggle />
        </header>
        {error && (
          <div className="banner danger">
            <AlertTriangle size={16} /> Can't reach the node: {String(error.message || error)} - retrying.
          </div>
        )}
        <main className="content">
          {/* Update reads its query (?extensions) when it mounts: a new query, a new instance. */}
          <View key={current.view === Update ? route : current.path} {...(current.props || {})} route={route} navigate={navigate} />
        </main>
      </div>
      {navOpen && <div className="nav-backdrop" onClick={() => setNavOpen(false)} />}
    </div>
  )
}
