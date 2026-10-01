import { ExternalLink, Workflow } from 'lucide-react'
import { Badge, Card, ErrorBox, Loading, PageHeader } from '../../shared/ui.jsx'
import ModuleConfigEditor from '../components/ModuleConfigEditor.jsx'
import { usePoll } from '../hooks.jsx'
import Module from './Module.jsx'

const DOCS = 'https://github.com/swenske/Janus/blob/main/docs/bgp.md'

// A starting point: checked by the extension's own BIRD.
const STARTER = `# bird.conf - see docs/bgp.md.
# Protocols named haproxy_* are kept down while this node's HAProxy
# doesn't answer: put the routes HAProxy serves (an anycast address) there.

router id 192.0.2.1;            # this node's own address

protocol device {}

# The anycast address HAProxy serves - also an address of this node
# (network configuration) - withdrawn while HAProxy doesn't answer.
protocol static haproxy_anycast {
    ipv4;
    route 192.0.2.100/32 blackhole;
}

protocol bgp upstream {
    local 192.0.2.1 as 65001;
    neighbor 192.0.2.254 as 65000;
    # password "secret";        # TCP MD5, if the peer asks for one
    ipv4 {
        import none;
        export where proto = "haproxy_anycast";
    };
}
`

const TONES = { up: 'ok', start: 'warn', down: '', stop: 'warn' }

function routes(p) {
  if (!p.channels.length) return '–'
  return p.channels.map((c) => `${c.name} ${c.imported} in / ${c.exported} out`).join(', ')
}

export default function BGP() {
  const status = usePoll('/api/network/bgp', { every: 3000 })
  const st = status.data
  if (st?.state === 'not_enabled') return <Module module="bgp" />
  const held = st?.protocols.filter((p) => p.held_down) || []
  return (
    <>
      <PageHeader
        title="BGP · bird"
        subtitle="Announce this node's addresses to your routers - an anycast address withdrawn while HAProxy doesn't answer"
        actions={
          <a className="button small" href={DOCS} target="_blank" rel="noreferrer">
            BGP guide <ExternalLink size={12} />
          </a>
        }
      />
      {status.error && <ErrorBox error={status.error} />}
      <Card
        title="Protocols"
        icon={Workflow}
        actions={
          st && (
            <div className="row">
              {st.version && (
                <span className="small muted">
                  BIRD {st.version} · router ID <span className="mono">{st.router_id}</span>
                </span>
              )}
              <Badge tone={st.haproxy_healthy ? 'ok' : 'danger'} dot>
                HAProxy {st.haproxy_healthy ? 'answers' : 'not answering'}
              </Badge>
            </div>
          )
        }
      >
        {!st ? (
          <Loading />
        ) : !st.configured ? (
          <span className="muted small">No bird.conf is saved: BIRD doesn't run. Write one below.</span>
        ) : st.error ? (
          <div className="error-box">{st.error}</div>
        ) : !st.protocols.length ? (
          <span className="muted small">BIRD is starting…</span>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Protocol</th>
                  <th>Type</th>
                  <th>State</th>
                  <th>Info</th>
                  <th>Neighbor</th>
                  <th>Routes</th>
                  <th>Since</th>
                </tr>
              </thead>
              <tbody>
                {st.protocols.map((p) => (
                  <tr key={p.name}>
                    <td className="mono">{p.name}</td>
                    <td>{p.protocol}</td>
                    <td>
                      <Badge tone={TONES[p.state]} dot>
                        {p.state}
                      </Badge>
                    </td>
                    <td className="small">
                      {p.held_down ? (
                        <Badge tone="danger">held down: HAProxy doesn't answer</Badge>
                      ) : (
                        <>
                          {p.info}
                          {p.last_error && !p.info.includes(p.last_error) && <div className="muted">{p.last_error}</div>}
                        </>
                      )}
                    </td>
                    <td className="mono small">{p.neighbor_address ? `${p.neighbor_address} AS${p.neighbor_as}` : '–'}</td>
                    <td className="small">{routes(p)}</td>
                    <td className="small mono">{p.since}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {st && (
          <p className="small muted" style={{ marginBottom: 0 }}>
            {held.length > 0
              ? `${held.map((p) => p.name).join(', ')} held down until HAProxy answers again. `
              : ''}
            Protocols named <span className="mono">haproxy_*</span> are kept down while this node's HAProxy doesn't answer. With the firewall extension, accept BGP:{' '}
            <span className="mono">tcp dport 179 accept</span>.
          </p>
        )}
      </Card>
      <div style={{ marginTop: '1rem' }}>
        <ModuleConfigEditor
          base="/api/network/bgp"
          file="bird.conf"
          daemon="BIRD"
          starter={STARTER}
          applyNote="BIRD checks it, then reconfigures: sessions whose configuration doesn't change stay up."
          onApplied={status.reload}
        />
      </div>
    </>
  )
}
