import {
  Activity,
  AlertTriangle,
  Archive,
  BarChart3,
  Boxes,
  Cable,
  Cpu,
  FolderOpen,
  HardDrive,
  KeyRound,
  LayoutDashboard,
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
import Module from './views/Module.jsx'
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
      { path: '/apps/bgp', label: 'BGP · bird', icon: Workflow, view: Module, props: { module: 'bgp' }, moduleKey: 'bgp' },
      { path: '/apps/vrrp', label: 'VRRP · keepalived', icon: Boxes, view: Module, props: { module: 'vrrp' }, moduleKey: 'vrrp' },
      { path: '/apps/firewall', label: 'Firewall · nftables', icon: Shield, view: Firewall, moduleKey: 'firewall' },
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
            <div className="muted small">Controller</div>
          </div>
        </div>
        <nav>
          {NAV.map((g) => (
            <div key={g.group} className="nav-group">
              <div className="nav-group-title">{g.group}</div>
              {g.items.map((item) => {
                const active = item === current
                const moduleState = item.moduleKey && modules.data?.[item.moduleKey]?.state
                return (
                  <a key={item.path} href={`#${item.path}`} className={`nav-item ${active ? 'active' : ''}`}>
                    <item.icon size={16} />
                    <span className="grow">{item.label}</span>
                    {moduleState === 'not_enabled' && <span className="nav-hint">n/a</span>}
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
          <View {...(current.props || {})} route={route} navigate={navigate} />
        </main>
      </div>
      {navOpen && <div className="nav-backdrop" onClick={() => setNavOpen(false)} />}
    </div>
  )
}
