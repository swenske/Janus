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
  FileArchive,
  FolderOpen,
  Gauge,
  HardDrive,
  KeyRound,
  LayoutDashboard,
  Lock,
  LockKeyhole,
  Menu,
  Network,
  Pin,
  PinOff,
  Power,
  Radar,
  Route,
  ScrollText,
  Send,
  Server,
  Shield,
  Shuffle,
  SlidersHorizontal,
  Stethoscope,
  Terminal,
  UserRound,
  Waypoints,
  Workflow,
} from 'lucide-react'
import { Suspense, useCallback, useEffect, useState } from 'react'
import { SIGNED_OUT } from '../shared/activity.js'
import BrandMark from '../shared/BrandMark.jsx'
import RefreshButton from '../shared/RefreshButton.jsx'
import ThemePicker from '../shared/ThemePicker.jsx'
import { Badge, Loading } from '../shared/ui.jsx'
import { lazyView } from '../shared/lazy.jsx'
import { useSplash, useSplashStep } from '../shared/Splash.jsx'
import { PrereleaseBadge } from '../PrereleaseBadge.jsx'
import { SecurityBadge } from '../SecurityBadge.jsx'
import { securityText, securityTone } from '../severity.js'
import { INTERVALS, navigate, useHashRoute, useMetrics, useNodeStatus, usePoll, useRefresh } from './hooks.jsx'
import { MayContext } from './may.js'
import { usePinned } from './rail.js'
import Events from './views/Events.jsx'
import Metrics from './views/Metrics.jsx'
import NetworkView from './views/Network.jsx'
import Overview from './views/Overview.jsx'
import PowerView from './views/Power.jsx'
import Processes from './views/Processes.jsx'
import Services from './views/Services.jsx'
import Storage from './views/Storage.jsx'

// The views a page opens on - the overview, the metrics, the small
// monitoring ones - come with the page; the rest in their own chunks,
// fetched when first shown (shared/lazy.jsx). Twenty-nine views in one
// bundle made it 627 KB.
const Access = lazyView(() => import('./views/Access.jsx'))
const Capture = lazyView(() => import('./views/Capture.jsx'))
const Files = lazyView(() => import('./views/Files.jsx'))
const Flow = lazyView(() => import('./views/Flow.jsx'))
const HAProxy = lazyView(() => import('./views/HAProxy.jsx'))
const Kernel = lazyView(() => import('./views/Kernel.jsx'))
const LogForwarding = lazyView(() => import('./views/LogForwarding.jsx'))
const Logs = lazyView(() => import('./views/Logs.jsx'))
const Firewall = lazyView(() => import('./views/Firewall.jsx'))
const GuestAgent = lazyView(() => import('./views/GuestAgent.jsx'))
const JanusExporter = lazyView(() => import('./views/JanusExporter.jsx'))
const NodeExporter = lazyView(() => import('./views/NodeExporter.jsx'))
const VRRP = lazyView(() => import('./views/VRRP.jsx'))
const BGP = lazyView(() => import('./views/BGP.jsx'))
const Consul = lazyView(() => import('./views/Consul.jsx'))
const LetsEncrypt = lazyView(() => import('./views/LetsEncrypt.jsx'))
const NetworkConfig = lazyView(() => import('./views/NetworkConfig.jsx'))
const Probe = lazyView(() => import('./views/Probe.jsx'))
const Support = lazyView(() => import('./views/Support.jsx'))
const Sysctl = lazyView(() => import('./views/Sysctl.jsx'))
const Update = lazyView(() => import('./views/Update.jsx'))

const NAV = [
  {
    group: 'Monitoring',
    items: [
      { path: '/', label: 'Overview', icon: LayoutDashboard, view: Overview },
      { path: '/metrics', label: 'Metrics', icon: BarChart3, view: Metrics },
      { path: '/flow', label: 'Traffic flow', icon: Route, view: Flow, needs: 'HAProxyService/Stats' },
      { path: '/processes', label: 'Processes', icon: Cpu, view: Processes },
      { path: '/network', label: 'Network', icon: Network, view: NetworkView },
      { path: '/storage', label: 'Storage', icon: HardDrive, view: Storage },
    ],
  },
  {
    group: 'Logs',
    items: [
      { path: '/logs', label: 'Service logs', icon: ScrollText, view: Logs, needs: 'SystemService/Logs' },
      { path: '/events', label: 'Events', icon: Activity, view: Events },
      { path: '/kernel', label: 'Kernel (dmesg)', icon: Terminal, view: Kernel, needs: 'SystemService/Dmesg' },
      { path: '/log-forwarding', label: 'Forwarding', icon: Send, view: LogForwarding, needs: 'SystemService/LogForwardingGet' },
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
      { path: '/apps/letsencrypt', label: "Let's Encrypt", icon: LockKeyhole, view: LetsEncrypt, extension: ['letsencrypt'], needs: 'HAProxyService/ACMEStatus' },
      { path: '/apps/consul', label: 'Consul', icon: Waypoints, view: Consul, extension: ['consul'] },
      { path: '/system/update?extensions', label: 'Add or remove apps…', icon: CirclePlus, link: true, needs: 'LifecycleService/Upgrade' },
    ],
  },
  {
    group: 'Tools',
    items: [
      { path: '/tools/capture', label: 'Packet capture', icon: Radar, view: Capture, needs: 'SystemService/PacketCapture' },
      { path: '/tools/files', label: 'Files', icon: FolderOpen, view: Files, needs: 'SystemService/List' },
      { path: '/tools/probe', label: 'Connectivity test', icon: Stethoscope, view: Probe, needs: 'SystemService/Probe' },
      { path: '/tools/support', label: 'Diagnostic bundle', icon: FileArchive, view: Support, needs: 'SystemService/Version' },
    ],
  },
  {
    group: 'System',
    items: [
      { path: '/system/network', label: 'Network', icon: Cable, view: NetworkConfig },
      { path: '/system/sysctl', label: 'Sysctl', icon: SlidersHorizontal, view: Sysctl, needs: 'SystemService/SysctlList' },
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
  const { recheck } = useNodeStatus()
  return (
    <div className="refresh">
      <RefreshButton onRefresh={() => Promise.all([refresh(), recheck()])} />
      <ThemePicker />
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
  const [pinned, togglePinned] = usePinned()
  const { latest, error } = useMetrics()
  const node = usePoll('/api/node', { every: 0 })
  const me = usePoll('/api/me', { every: 0 })
  const [ended, setEnded] = useState(false)
  useEffect(() => {
    const onEnded = () => setEnded(true)
    window.addEventListener(SIGNED_OUT, onEnded)
    return () => window.removeEventListener(SIGNED_OUT, onEnded)
  }, [])
  const role = (me.data?.roles?.[0] || '').replace(/^os:/, '')
  // What the account may call on the node: nothing until /api/me says -
  // never a flash of what it may not do, nor a read it would be refused.
  const mayList = me.data?.may
  const may = useCallback((method) => !!mayList && mayList.includes(method), [mayList])
  const modules = usePoll('/api/network/modules', { every: 0 })
  const { check, overview } = useNodeStatus()

  useEffect(() => setNavOpen(false), [route])
  const name = node.data?.name || 'Janus node'
  useEffect(() => {
    document.title = `${name} · Janus`
  }, [name])
  // The splash until the node first answers; a node that doesn't leaves
  // the page to say so (the banner below).
  const { finish } = useSplash()
  useSplashStep('node', 'Connecting to the node…', node.data || node.error)
  useSplashStep('me', `Checking your access to ${name}…`, me.data || me.error)
  useSplashStep('overview', `Reading ${name}'s system…`, overview.data || overview.error)
  useSplashStep('metrics', `First metrics from ${name}…`, latest || error)
  useEffect(() => {
    if (error || node.error) finish()
  }, [error, node.error, finish])

  const version = overview.data?.version?.version
  const extensions = (overview.data?.version?.extensions || []).map((e) => e.name)
  // Apps the node doesn't have aren't listed: they'd only say "n/a".
  const shown = (item) => {
    if (item.needs && !may(item.needs)) return false
    if (item.moduleKey) return !!modules.data && modules.data[item.moduleKey]?.state !== 'not_enabled'
    if (item.extension) return item.extension.some((n) => extensions.includes(n))
    return true
  }
  const latestTag = check.data?.latest
  const updateAvailable = check.data?.state === 'ready' && check.data?.update_available
  const securityUpdate = check.data?.security_update
  const hap = latest?.hap
  const View = current.view
  const allowed = !current.needs || may(current.needs)

  return (
    <MayContext.Provider value={may}>
    <div className={`layout ${navOpen ? 'nav-open' : ''} ${pinned ? 'pinned' : ''}`}>
      <div className="rail">
      <aside className="sidebar" aria-label="Node">
        <button
          className="skip-link"
          onClick={() => document.getElementById('content')?.focus()}
        >
          Skip to the page
        </button>
        <div className="sidebar-brand">
          <BrandMark size={26} />
          <div className="brand-text">
            <div className="brand-name">Janus</div>
            <div className="muted small">Controller{node.data?.controller_version ? <span className="mono"> {node.data.controller_version}</span> : null}</div>
          </div>
        </div>
        {me.data && (
          <a href="/" className="user-chip" title="The Controller account this page acts for - back to the Controller">
            <UserRound size={14} />
            <span className="chip-text">
              {me.data.name} {role && <Badge>{role}</Badge>}
            </span>
          </a>
        )}
        <nav>
          {NAV.filter((g) => g.items.some(shown)).map((g) => (
            <div key={g.group} className="nav-group">
              <div className="nav-group-title">{g.group}</div>
              {g.items.filter(shown).map((item) => {
                const active = item === current
                return (
                  <a key={item.path} href={`#${item.path}`} className={`nav-item ${active ? 'active' : ''} ${item.link ? 'nav-link' : ''}`}>
                    <item.icon size={16} />
                    <span className="grow nav-label">{item.label}</span>
                    {item.path === '/system/update' && (securityUpdate || updateAvailable) && (
                      <span
                        className={`nav-dot ${securityUpdate ? securityTone(securityUpdate) : ''}`}
                        title={securityUpdate ? securityText(securityUpdate) : 'Update available'}
                      />
                    )}
                  </a>
                )
              })}
            </div>
          ))}
        </nav>
        <button className="ghost rail-pin" aria-pressed={pinned} title={pinned ? 'Let the sidebar fold into icons' : 'Keep the sidebar open'} onClick={togglePinned}>
          {pinned ? <PinOff size={16} /> : <Pin size={16} />}
          <span className="nav-label">{pinned ? 'Fold the sidebar' : 'Keep it open'}</span>
        </button>
      </aside>
      </div>
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
              <a
                href="#/system/update"
                className="version-link"
                title={securityUpdate ? securityText(securityUpdate) : updateAvailable ? `Update available: ${latestTag}` : 'Up to date'}
              >
                <Badge tone={updateAvailable ? 'accent' : ''}>{version}</Badge>
                {check.data?.running_prerelease && <PrereleaseBadge />}
                {securityUpdate ? (
                  <SecurityBadge severity={securityUpdate}>security update → {latestTag}</SecurityBadge>
                ) : (
                  updateAvailable && <Badge tone="accent">update → {latestTag}</Badge>
                )}
              </a>
            )}
          </div>
          <span className="grow" />
          <RefreshSelect />
        </header>
        {error && !ended && (
          <div className="banner danger">
            <AlertTriangle size={16} /> Can't reach the node: {String(error.message || error)} - retrying.
          </div>
        )}
        {node.data?.locked_by && (
          <div className="banner info">
            <Lock size={16} /> Managed by {node.data.locked_by} and locked: its network and updates are changed there - this page refuses them. Release it on the Controller&apos;s
            Hypervisors tab to change them here.
          </div>
        )}
        {role === 'reader' && <div className="banner info">You&apos;re a reader: this page shows the node, and offers nothing that changes it.</div>}
        {ended && (
          <div className="banner warn">
            Your session on the Controller ended. <a href="/">Sign in again</a>, then reload this page.
          </div>
        )}
        <main className="content" id="content" tabIndex={-1}>
          {/* Update reads its query (?extensions) when it mounts: a new query, a new instance. */}
          {!me.data && !me.error ? (
            <Loading />
          ) : allowed ? (
            <Suspense fallback={<Loading />}>
              <View key={current.view === Update ? route : current.path} {...(current.props || {})} route={route} navigate={navigate} />
            </Suspense>
          ) : (
            <div className="notice">{current.label}: your role ({role}) doesn&apos;t reach it on this node.</div>
          )}
        </main>
      </div>
      {navOpen && <div className="nav-backdrop" onClick={() => setNavOpen(false)} />}
    </div>
    </MayContext.Provider>
  )
}
