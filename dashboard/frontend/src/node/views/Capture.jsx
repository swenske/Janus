import { Download, HelpCircle, Radar } from 'lucide-react'
import { useEffect, useState } from 'react'
import { download } from '../api.js'
import { Card, PageHeader, useAction } from '../../shared/ui.jsx'
import { bytes } from '../format.js'
import { usePoll } from '../hooks.jsx'

const EXAMPLES = ['tcp port 443', 'host 10.0.0.5 and port 8080', 'not port 22', 'udp port 53', 'net 192.168.0.0/16 and tcp', 'icmp or icmp6']

export default function Capture() {
  const devices = usePoll('/api/system/netdev', { every: 0 })
  const ifaces = (devices.data?.devices || []).map((d) => d.name)
  const [iface, setIface] = useState('')
  const [filter, setFilter] = useState('')
  const [duration, setDuration] = useState(10)
  const [snaplen, setSnaplen] = useState('')
  const [promisc, setPromisc] = useState(false)
  const [includeOwn, setIncludeOwn] = useState(false)
  const [remaining, setRemaining] = useState(0)
  const [history, setHistory] = useState([])
  const [busy, run] = useAction()

  useEffect(() => {
    if (!iface && ifaces.length) setIface(ifaces.find((n) => n !== 'lo') || ifaces[0])
  }, [ifaces, iface])
  useEffect(() => {
    if (!busy) return undefined
    setRemaining(duration)
    const t = setInterval(() => setRemaining((r) => Math.max(0, r - 1)), 1000)
    return () => clearInterval(t)
  }, [busy, duration])

  const start = () =>
    run(async () => {
      const q = new URLSearchParams({ interface: iface, filter, duration: String(duration) })
      if (snaplen) q.set('snaplen', snaplen)
      if (promisc) q.set('promisc', 'true')
      if (includeOwn) q.set('include_own_stream', 'true')
      const r = await download(`/api/pcap?${q}`, 'capture.pcap')
      setHistory((h) => [{ ...r, at: new Date(), iface, filter, duration }, ...h].slice(0, 10))
      return r
    }, (r) => `Saved ${r.name} (${bytes(r.size)})`)

  return (
    <>
      <PageHeader title="Packet capture" subtitle="Capture live traffic on the node and download it as a .pcap file for Wireshark or tcpdump" />
      <div className="grid grid-2">
        <Card title="New capture" icon={Radar}>
          <div className="stack">
            <div className="grid grid-3">
              <label className="field">
                <span>Interface</span>
                <select value={iface} onChange={(e) => setIface(e.target.value)}>
                  {ifaces.map((n) => (
                    <option key={n}>{n}</option>
                  ))}
                </select>
              </label>
              <label className="field">
                <span>Duration (1-300 s)</span>
                <input type="number" min={1} max={300} value={duration} onChange={(e) => setDuration(Math.max(1, Math.min(300, Number(e.target.value) || 1)))} />
              </label>
              <label className="field">
                <span>Snap length (bytes)</span>
                <input type="number" min={0} placeholder="whole packets" value={snaplen} onChange={(e) => setSnaplen(e.target.value)} />
              </label>
            </div>
            <label className="field">
              <span>Filter (tcpdump syntax, empty = everything)</span>
              <input className="mono" placeholder="tcp port 443 and not host 10.0.0.1" value={filter} onChange={(e) => setFilter(e.target.value)} />
            </label>
            <div className="chips">
              {EXAMPLES.map((ex) => (
                <button key={ex} className="small chip mono" onClick={() => setFilter(ex)}>
                  {ex}
                </button>
              ))}
            </div>
            <div className="row">
              <label className="check">
                <input type="checkbox" checked={promisc} onChange={(e) => setPromisc(e.target.checked)} /> Promiscuous mode
              </label>
              <label className="check" title="Debugging only: the capture then captures itself and grows very fast">
                <input type="checkbox" checked={includeOwn} onChange={(e) => setIncludeOwn(e.target.checked)} /> Include this capture's own stream
              </label>
            </div>
            <div className="row">
              <button className="primary" disabled={busy || !iface} onClick={start}>
                <Download size={15} /> {busy ? (remaining > 0 ? `Capturing… ${remaining}s` : 'Finishing…') : 'Capture & download .pcap'}
              </button>
            </div>
          </div>
        </Card>
        <Card title="Filter syntax" icon={HelpCircle}>
          <div className="stack small">
            <div>
              Protocols: <code>ip</code> <code>ip6</code> <code>arp</code> <code>tcp</code> <code>udp</code> <code>icmp</code> <code>icmp6</code>
            </div>
            <div>
              Addresses & ports, optionally with <code>src</code>/<code>dst</code>: <code>host 10.0.0.5</code>, <code>net 10.0.0.0/8</code>, <code>port 443</code>,{' '}
              <code>portrange 8000-8999</code>
            </div>
            <div>
              Combine with <code>and</code>, <code>or</code>, <code>not</code> and parentheses. Like tcpdump, <code>and</code> and <code>or</code> have the same priority,
              left to right: use parentheses when mixing them.
            </div>
            <div className="muted">
              The capture's own connection to the node is always left out. Filtering happens in the node's kernel: packets that don't match never leave it. The whole
              capture is held in your browser until it's saved - prefer a precise filter on a busy node.
            </div>
          </div>
        </Card>
      </div>
      {history.length > 0 && (
        <Card title="This session's captures" icon={Download} className="" >
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>File</th>
                  <th>Interface</th>
                  <th>Filter</th>
                  <th className="num">Duration</th>
                  <th className="num">Size</th>
                </tr>
              </thead>
              <tbody>
                {history.map((h, i) => (
                  <tr key={i}>
                    <td className="mono small">{h.name}</td>
                    <td className="mono">{h.iface}</td>
                    <td className="mono small">{h.filter || <span className="muted">everything</span>}</td>
                    <td className="num">{h.duration}s</td>
                    <td className="num">{bytes(h.size)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>
      )}
    </>
  )
}
