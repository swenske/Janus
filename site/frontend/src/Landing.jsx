import {
  ArrowRight,
  BookOpen,
  Boxes,
  Download,
  Feather,
  Gauge,
  GitBranch,
  Hammer,
  KeyRound,
  Layers,
  Lock,
  Network,
  RefreshCcw,
  Server,
  ShieldCheck,
  Terminal,
  Undo2,
  Workflow,
} from 'lucide-react'
import { useEffect, useState } from 'react'
import { Badge, Card } from '@shared/ui.jsx'
import { DOCS, REPO, getJSON } from './api.js'
import { Link } from './App.jsx'

const PILLARS = [
  {
    icon: ShieldCheck,
    title: 'Secure by construction',
    points: [
      'Read-only root verified block by block with dm-verity; the root hash sits in a Secure Boot-signed kernel image.',
      'SELinux enforcing, with a hand-written policy confining janusd, HAProxy and every extension.',
      'No shell, no SSH, no package manager: nothing to log into.',
      'Every API call over mutual TLS, with admin and read-only roles.',
      'Updates are installed only when signed with a Janus release key.',
    ],
  },
  {
    icon: Feather,
    title: 'Light',
    points: [
      'A 13 MB root filesystem: a Linux kernel, a Go init, janusd and HAProxy - nothing else unless you add it.',
      'A 42 MB compressed VM disk; boots straight to a serving HAProxy.',
      'Built from scratch (LFS-style), from a kernel configured symbol by symbol.',
      'Optional software is chosen per image, not installed: an image carries exactly what you picked.',
    ],
  },
  {
    icon: Workflow,
    title: 'Managed through an API',
    points: [
      'A gRPC API for everything: HAProxy configuration, maps, ACLs, certificates, backends, network, logs, packet capture, updates.',
      'janusctl on the command line; the Janus Controller in the browser, for all your nodes.',
      'A/B updates with automatic rollback when HAProxy doesn’t come back healthy.',
      'Network changes applied on trial: one that cuts the node off reverts by itself.',
    ],
  },
]

const FEATURES = [
  { icon: Server, title: 'HAProxy 3.4', text: 'Static, with AWS-LC for fast TLS and the Prometheus exporter. Seamless reloads, runtime maps, ACLs and certificates through the API.' },
  { icon: Undo2, title: 'A/B updates', text: 'The new version is written to the idle slot and booted on trial; if HAProxy isn’t healthy, the node switches back by itself.' },
  { icon: Network, title: 'Network', text: 'Static addressing, 802.1Q VLANs, several interfaces, DNS and NTP - configured through the API, confirmed or reverted.' },
  { icon: Terminal, title: 'No shell, still observable', text: 'Logs, kernel messages, processes, files and packet capture over the API - read-only and scoped, never a command line.' },
  { icon: Boxes, title: 'Extensions', text: 'node_exporter, the QEMU guest agent - more to come. Pick them in the builder: they are part of the signed image.' },
  { icon: KeyRound, title: 'Provisioning', text: 'Nodes register with your Controller on first boot. Controller and network settings at install, offline on an image, or from a NoCloud volume.' },
]

function LatestRelease() {
  const [rel, setRel] = useState(null)
  useEffect(() => {
    getJSON('/api/v1/versions')
      .then((list) => setRel(list?.[0] || null))
      .catch(() => {})
  }, [])
  if (!rel) return null
  return (
    <a className="release-pill" href={rel.url}>
      <Badge tone="accent">new</Badge> {rel.version} is out <ArrowRight size={14} />
    </a>
  )
}

export default function Landing() {
  return (
    <>
      <section className="hero">
        <LatestRelease />
        <h1>
          An immutable Linux
          <br />
          for your <span className="accent">HAProxy</span> load balancers
        </h1>
        <p className="lead">
          Janus is a tiny, API-driven operating system built around HAProxy. It boots a verified, read-only image, has no shell to break into, and updates itself atomically - with a
          rollback when something goes wrong.
        </p>
        <div className="row hero-actions">
          <Link to="/builder" className="button primary big">
            <Hammer size={17} /> Build your image
          </Link>
          <a className="button big" href={`${REPO}/releases/latest`}>
            <Download size={17} /> Latest release
          </a>
          <a className="button big ghostish" href={REPO}>
            <GitBranch size={17} /> Source
          </a>
        </div>
        <div className="hero-facts">
          <div>
            <strong>13 MB</strong>
            <span>root filesystem</span>
          </div>
          <div>
            <strong>0</strong>
            <span>shells, SSH daemons, package managers</span>
          </div>
          <div>
            <strong>mTLS</strong>
            <span>on every API call</span>
          </div>
          <div>
            <strong>A/B</strong>
            <span>updates, signed, with rollback</span>
          </div>
        </div>
      </section>

      <section className="section">
        <div className="grid grid-3">
          {PILLARS.map((p) => (
            <Card key={p.title} className="pillar">
              <div className="pillar-icon">
                <p.icon size={22} />
              </div>
              <h2>{p.title}</h2>
              <ul>
                {p.points.map((pt) => (
                  <li key={pt}>{pt}</li>
                ))}
              </ul>
            </Card>
          ))}
        </div>
      </section>

      <section className="section">
        <h2 className="section-title">Everything a load balancer needs, nothing else</h2>
        <div className="grid grid-3">
          {FEATURES.map((f) => (
            <div key={f.title} className="feature">
              <f.icon size={18} className="feature-icon" />
              <div>
                <h3>{f.title}</h3>
                <p className="muted">{f.text}</p>
              </div>
            </div>
          ))}
        </div>
      </section>

      <section className="section">
        <h2 className="section-title">From image to fleet</h2>
        <div className="steps">
          <div className="step">
            <span className="step-n">1</span>
            <h3>Build</h3>
            <p className="muted">
              Choose your platform and extensions in the <Link to="/builder">image builder</Link>. Your choices get a schematic ID - the same choices, the same ID.
            </p>
          </div>
          <div className="step">
            <span className="step-n">2</span>
            <h3>Boot</h3>
            <p className="muted">Import the disk in Proxmox, KVM or VMware, flash the ISO or the Raspberry Pi image. The node prints its admin credentials once, on its console.</p>
          </div>
          <div className="step">
            <span className="step-n">3</span>
            <h3>Manage</h3>
            <p className="muted">
              Add it to the <a href={`${DOCS}/dashboard/README.md`}>Janus Controller</a>, or drive it with <a href={`${DOCS}/janusctl.md#installing-janusctl`}>janusctl</a> (<code>apt install janusctl</code>): HAProxy configuration, certificates, network, monitoring.
            </p>
          </div>
          <div className="step">
            <span className="step-n">4</span>
            <h3>Update</h3>
            <p className="muted">The Controller finds the update built for the node&apos;s schematic and installs it on the idle slot - extensions included.</p>
          </div>
        </div>
      </section>

      <section className="section">
        <div className="grid grid-2">
          <Card title="Documentation" icon={BookOpen}>
            <ul className="links">
              <li>
                <a href={`${DOCS}/architecture.md`}>Architecture</a> <span className="muted">- immutability, trusted boot, PKI, SELinux</span>
              </li>
              <li>
                <a href={`${DOCS}/provisioning-a-node.md`}>Provisioning a node</a> <span className="muted">- install, Controller registration, NoCloud</span>
              </li>
              <li>
                <a href={`${DOCS}/image-factory.md`}>Image schematics and extensions</a>
              </li>
              <li>
                <a href={`${DOCS}/network-configuration.md`}>Network configuration</a> <span className="muted">- interfaces, VLANs, DNS, NTP</span>
              </li>
              <li>
                <a href={`${DOCS}/packet-capture.md`}>Packet capture</a>
              </li>
              <li>
                <a href={`${DOCS}/api-routes.md`}>API reference</a>
              </li>
            </ul>
          </Card>
          <Card title="Get Janus" icon={Layers}>
            <ul className="links">
              <li>
                <Link to="/builder">Image builder</Link> <span className="muted">- your platform, your extensions</span>
              </li>
              <li>
                <a href={`${REPO}/releases`}>Releases</a> <span className="muted">- default images, update bundles, release notes</span>
              </li>
              <li>
                <a href="https://hub.docker.com/r/swenske/janus-controller">Janus Controller</a> <span className="muted">- Docker image of the web UI</span>
              </li>
              <li>
                <a href={REPO}>Source code</a> <span className="muted">- kernel config, init, janusd, SELinux policy, build pipeline</span>
              </li>
              <li>
                <a href={`${REPO}/issues`}>Issues</a>
              </li>
            </ul>
            <div className="notice warn" style={{ marginTop: '0.8rem' }}>
              <Gauge size={14} /> Janus is alpha software: expect breaking changes.
            </div>
          </Card>
        </div>
      </section>

      <section className="section cta">
        <Lock size={20} />
        <h2>Ready to try it?</h2>
        <Link to="/builder" className="button primary big">
          <RefreshCcw size={16} /> Open the image builder
        </Link>
      </section>
    </>
  )
}
