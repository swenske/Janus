import { CircleCheck, CircleX, Play, Square, Stethoscope } from 'lucide-react'
import { useRef, useState } from 'react'
import { ApiError } from '../api.js'
import { apiURL } from '../../shared/base.js'
import { Badge, Card, ErrorBox, PageHeader } from '../../shared/ui.jsx'
import { dateTime } from '../format.js'
import { usePoll } from '../hooks.jsx'
import { useMay } from '../may.js'

const KINDS = [
  { id: 'http', label: 'HTTP', placeholder: 'https://backend.example:8443/health', about: 'a request to a URL: DNS, connection, TLS, the answer - each timed' },
  { id: 'tls', label: 'TLS', placeholder: 'backend.example:443', about: 'a handshake: the certificate the server presents, verified' },
  { id: 'tcp', label: 'TCP', placeholder: '10.0.0.10:8080', about: 'a connection to host:port' },
  { id: 'dns', label: 'DNS', placeholder: 'backend.example', about: 'a lookup, with the node’s resolver or another server' },
  { id: 'ping', label: 'Ping', placeholder: '10.0.0.1', about: 'ICMP echo requests - the DF bit tests an MTU' },
  { id: 'traceroute', label: 'Traceroute', placeholder: 'backend.example', about: 'the routers on the way' },
]

// probe runs POST /api/system/probe, handing each event it streams
// (NDJSON) to onEvent.
async function probe(body, signal, onEvent) {
  const resp = await fetch(apiURL('/api/system/probe'), { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), signal })
  if (!resp.ok) throw new ApiError((await resp.text()).trim(), resp.status)
  const reader = resp.body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  for (;;) {
    const { value, done } = await reader.read()
    if (done) break
    buf += decoder.decode(value, { stream: true })
    for (let i = buf.indexOf('\n'); i >= 0; i = buf.indexOf('\n')) {
      const line = buf.slice(0, i).trim()
      buf = buf.slice(i + 1)
      if (!line) continue
      const ev = JSON.parse(line)
      if (ev.stage === 'error') throw new ApiError(ev.message, 502)
      onEvent(ev)
    }
  }
}

const ms = (v) => (v == null || v < 0 ? '*' : `${v < 10 ? v.toFixed(2) : v.toFixed(1)} ms`)

function Step({ ev }) {
  const icon = ev.ok ? <CircleCheck size={15} color="var(--ok)" /> : <CircleX size={15} color="var(--danger)" />
  let what = ev.message
  let extra = null
  if (ev.dns) what = ev.ok ? `${ev.dns.name} (${ev.dns.type}, ${ev.dns.server}): ${ev.dns.records.join(', ')}` : `${ev.dns.name}: ${ev.message}`
  if (ev.connect) what = ev.ok ? `${ev.connect.remote} from ${ev.connect.local}` : `${ev.connect.remote}: ${ev.message}`
  if (ev.tls) {
    const t = ev.tls
    what = (
      <>
        {t.version}, {t.cipher}
        {t.alpn ? `, ALPN ${t.alpn}` : ''}
        {t.server_name ? `, SNI ${t.server_name}` : ''} -{' '}
        {t.verified ? <Badge tone="ok">certificate verified</Badge> : <Badge tone="danger">not trusted: {t.verify_error}</Badge>}
        {t.ocsp && <> OCSP stapled: {t.ocsp}</>}
      </>
    )
    extra = (
      <div className="table-wrap" style={{ marginTop: '0.4rem' }}>
        <table>
          <thead>
            <tr>
              <th>Certificate</th>
              <th>Names</th>
              <th>Until</th>
              <th>Issuer</th>
            </tr>
          </thead>
          <tbody>
            {t.chain.map((c) => (
              <tr key={c.sha256}>
                <td className="mono small">{c.subject}</td>
                <td className="mono small">{[...c.dns_names, ...c.ips].join(', ') || '–'}</td>
                <td className="small">{dateTime(Number(c.not_after_unix) * 1000)}</td>
                <td className="mono small">{c.issuer}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    )
  }
  if (ev.http) {
    const h = ev.http
    what = `${h.proto} ${h.status} ${h.status_text}, ${h.body_truncated ? 'more than ' : ''}${Number(h.body_bytes)} bytes, first byte after ${ms(h.ttfb_ms)}`
    extra = (
      <details style={{ marginTop: '0.4rem' }}>
        <summary className="muted small">Headers and the body’s first KiB</summary>
        <div className="logview" style={{ marginTop: '0.4rem' }}>
          {h.headers.map((x, i) => (
            <div key={i} className="logline">
              {x.name}: {x.value}
            </div>
          ))}
          {h.body_preview && (
            <div className="logline" style={{ marginTop: '0.6rem', whiteSpace: 'pre-wrap' }}>
              {h.body_preview}
            </div>
          )}
        </div>
      </details>
    )
  }
  return (
    <div className="probe-step">
      <span className="row" style={{ gap: '0.35rem', flexWrap: 'nowrap' }}>
        {icon}
        <strong>{ev.stage}</strong>
      </span>
      <span className="muted small num">{ms(ev.elapsed_ms)}</span>
      <div>
        <div>{what}</div>
        {extra}
      </div>
    </div>
  )
}

function Replies({ events }) {
  const replies = events.filter((e) => e.reply).map((e) => ({ ...e.reply, message: e.message, ok: e.ok }))
  if (!replies.length) return null
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Seq</th>
            <th>From</th>
            <th>Answer</th>
            <th className="num">TTL</th>
            <th className="num">Bytes</th>
            <th className="num">Round trip</th>
          </tr>
        </thead>
        <tbody>
          {replies.map((r) => (
            <tr key={r.seq}>
              <td>{r.seq}</td>
              <td className="mono">{r.from || '–'}</td>
              <td>
                <Badge tone={r.ok ? 'ok' : r.kind === 'timeout' ? 'warn' : 'danger'}>{r.kind}</Badge>
                {r.mtu > 0 && <span className="muted small"> MTU {r.mtu}</span>}
                {r.message && <span className="muted small"> {r.message}</span>}
              </td>
              <td className="num">{r.ttl || '–'}</td>
              <td className="num">{r.bytes || '–'}</td>
              <td className="num">{r.ok ? ms(r.rtt_ms) : '–'}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function Hops({ events }) {
  const hops = events.filter((e) => e.hop).map((e) => ({ ...e.hop, message: e.message }))
  if (!hops.length) return null
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th className="num">Hop</th>
            <th>Router</th>
            <th className="num">Round trips</th>
          </tr>
        </thead>
        <tbody>
          {hops.map((h) => (
            <tr key={h.ttl}>
              <td className="num">{h.ttl}</td>
              <td>
                {h.address ? (
                  <>
                    <span className="mono">{h.address}</span>
                    {h.name && <span className="muted small"> {h.name}</span>}
                    {h.reached && (
                      <>
                        {' '}
                        <Badge tone="ok">target</Badge>
                      </>
                    )}
                  </>
                ) : (
                  <span className="muted">no answer</span>
                )}
              </td>
              <td className="num mono small">{h.rtts_ms.map(ms).join('  ')}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

export default function Probe() {
  const may = useMay()
  const net = usePoll('/api/network/status', { every: 0, enabled: may('NetworkService/NetworkStatus') })
  const [kind, setKind] = useState('http')
  const [target, setTarget] = useState('')
  const [opts, setOpts] = useState({ source: '', record_type: '', dns_server: '', connect_to: '', sni: '', method: 'GET', headers: '', ca_pem: '', count: 4, size: '', dont_fragment: false, max_hops: '' })
  const [events, setEvents] = useState([])
  const [error, setError] = useState(null)
  const [running, setRunning] = useState(false)
  const abort = useRef(null)
  const k = KINDS.find((x) => x.id === kind)
  const set = (field) => (e) => setOpts({ ...opts, [field]: e.target.type === 'checkbox' ? e.target.checked : e.target.value })
  const sources = (net.data?.interfaces || []).flatMap((i) => (i.addresses || []).map((a) => a.split('/')[0]))

  const run = async () => {
    const body = { kind, target: target.trim(), source: opts.source }
    if (kind === 'dns') Object.assign(body, { record_type: opts.record_type, dns_server: opts.dns_server })
    if (kind === 'tls' || kind === 'http') Object.assign(body, { connect_to: opts.connect_to, sni: opts.sni, ca_pem: opts.ca_pem })
    if (kind === 'http') {
      body.method = opts.method
      body.headers = opts.headers
        .split('\n')
        .map((l) => l.trim())
        .filter(Boolean)
        .map((l) => ({ name: l.split(':')[0].trim(), value: l.slice(l.indexOf(':') + 1).trim() }))
    }
    if (kind === 'ping') Object.assign(body, { count: Number(opts.count) || 0 })
    if (kind === 'ping' || kind === 'traceroute') Object.assign(body, { size: Number(opts.size) || 0, dont_fragment: opts.dont_fragment })
    if (kind === 'traceroute') body.max_hops = Number(opts.max_hops) || 0
    setEvents([])
    setError(null)
    setRunning(true)
    abort.current = new AbortController()
    try {
      await probe(body, abort.current.signal, (ev) => setEvents((all) => [...all, ev]))
    } catch (err) {
      if (err.name !== 'AbortError') setError(err)
    } finally {
      setRunning(false)
    }
  }

  const done = events.find((e) => e.stage === 'done')
  const steps = events.filter((e) => !e.reply && !e.hop && e.stage !== 'done')
  return (
    <>
      <PageHeader title="Connectivity test" subtitle="The network as this node sees it: its routes, its resolver, its addresses" />
      <Card title="Test" icon={Stethoscope}>
        <div className="stack">
          <div className="chips">
            {KINDS.map((x) => (
              <button key={x.id} className={`small chip ${kind === x.id ? 'active' : ''}`} onClick={() => setKind(x.id)} disabled={running}>
                {x.label}
              </button>
            ))}
          </div>
          <p className="muted small" style={{ margin: 0 }}>
            {k.about}
          </p>
          <form
            className="row"
            onSubmit={(e) => {
              e.preventDefault()
              if (target.trim() && !running) run()
            }}
          >
            <input className="mono grow" placeholder={k.placeholder} value={target} onChange={(e) => setTarget(e.target.value)} />
            {running ? (
              <button type="button" className="danger" onClick={() => abort.current?.abort()}>
                <Square size={15} /> Stop
              </button>
            ) : (
              <button className="primary" disabled={!target.trim()}>
                <Play size={15} /> Run
              </button>
            )}
          </form>
          <details>
            <summary className="muted small">Options</summary>
            <div className="grid grid-2" style={{ marginTop: '0.6rem' }}>
              <label className="field">
                <span>Source address</span>
                <input className="mono" list="probe-sources" placeholder="the route's" value={opts.source} onChange={set('source')} />
                <datalist id="probe-sources">
                  {sources.map((s) => (
                    <option key={s} value={s} />
                  ))}
                </datalist>
              </label>
              {kind === 'dns' && (
                <>
                  <label className="field">
                    <span>Record type</span>
                    <select value={opts.record_type} onChange={set('record_type')}>
                      <option value="">A and AAAA</option>
                      {['A', 'AAAA', 'CNAME', 'SRV', 'TXT', 'MX', 'NS', 'PTR'].map((t) => (
                        <option key={t}>{t}</option>
                      ))}
                    </select>
                  </label>
                  <label className="field">
                    <span>DNS server</span>
                    <input className="mono" placeholder="the node's resolv.conf" value={opts.dns_server} onChange={set('dns_server')} />
                  </label>
                </>
              )}
              {(kind === 'tls' || kind === 'http') && (
                <>
                  <label className="field">
                    <span>Connect to (instead of resolving)</span>
                    <input className="mono" placeholder="10.0.0.10 or 10.0.0.10:8443" value={opts.connect_to} onChange={set('connect_to')} />
                  </label>
                  <label className="field">
                    <span>SNI</span>
                    <input className="mono" placeholder="the target's host" value={opts.sni} onChange={set('sni')} />
                  </label>
                  <label className="field" style={{ gridColumn: '1 / -1' }}>
                    <span>Verify against this CA (PEM) rather than the system's</span>
                    <textarea className="mono" rows={2} value={opts.ca_pem} onChange={set('ca_pem')} />
                  </label>
                </>
              )}
              {kind === 'http' && (
                <>
                  <label className="field">
                    <span>Method</span>
                    <select value={opts.method} onChange={set('method')}>
                      <option>GET</option>
                      <option>HEAD</option>
                      <option>OPTIONS</option>
                    </select>
                  </label>
                  <label className="field">
                    <span>Headers, one per line</span>
                    <textarea className="mono" rows={2} placeholder="Host: www.example.com" value={opts.headers} onChange={set('headers')} />
                  </label>
                </>
              )}
              {kind === 'ping' && (
                <label className="field">
                  <span>Echo requests</span>
                  <input type="number" min={1} max={20} value={opts.count} onChange={set('count')} />
                </label>
              )}
              {kind === 'traceroute' && (
                <label className="field">
                  <span>Hops at most</span>
                  <input type="number" min={1} max={30} placeholder="30" value={opts.max_hops} onChange={set('max_hops')} />
                </label>
              )}
              {(kind === 'ping' || kind === 'traceroute') && (
                <>
                  <label className="field">
                    <span>Payload bytes</span>
                    <input type="number" min={0} max={9216} placeholder="56" value={opts.size} onChange={set('size')} />
                  </label>
                  <label className="check">
                    <input type="checkbox" checked={opts.dont_fragment} onChange={set('dont_fragment')} /> Don’t fragment (an MTU test)
                  </label>
                </>
              )}
            </div>
          </details>
        </div>
      </Card>
      {(events.length > 0 || error || running) && (
        <Card
          title="Result"
          icon={Stethoscope}
          actions={done ? <Badge tone={done.ok ? 'ok' : 'danger'}>{done.message}</Badge> : running ? <Badge tone="info">running…</Badge> : null}
        >
          <div className="stack">
            <ErrorBox error={error?.status === 501 ? new Error('This node’s Janus is older than connectivity tests: update it (System › Update).') : error} />
            {steps.map((ev, i) => (
              <Step key={i} ev={ev} />
            ))}
            <Replies events={events} />
            <Hops events={events} />
            {done?.summary && done.summary.received > 0 && (
              <p className="muted small" style={{ margin: 0 }}>
                Round trips min/avg/max: {ms(done.summary.min_ms)} / {ms(done.summary.avg_ms)} / {ms(done.summary.max_ms)}
              </p>
            )}
          </div>
        </Card>
      )}
    </>
  )
}
