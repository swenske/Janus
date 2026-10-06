import { Boxes, ExternalLink } from 'lucide-react'
import { Badge, Card, ErrorBox, Loading, PageHeader } from '../../shared/ui.jsx'
import ModuleConfigEditor from '../components/ModuleConfigEditor.jsx'
import { usePoll } from '../hooks.jsx'
import Module from './Module.jsx'

// Its page on the docs site, by the file's permalink.
const DOCS = 'https://janus.sw-servers.net/docs/vrrp.md'

// A starting point: checked by the extension's own keepalived.
const STARTER = `# keepalived.conf - see docs/vrrp.md.
# Every node of the group gets the same file, but for its priority.

track_file haproxy {
    # 0 while this node's HAProxy answers, 1 when it doesn't - kept by janusd.
    file /run/janus/keepalived/haproxy-health
}

vrrp_instance VI_1 {
    state BACKUP
    interface eth0
    virtual_router_id 51
    priority 100            # higher wins: give each node its own
    advert_int 1
    track_file {
        haproxy weight 0    # HAProxy not answering: give the virtual IPs up
    }
    virtual_ipaddress {
        192.0.2.100/24
    }
}
`

const TONES = { MASTER: 'ok', BACKUP: 'info', FAULT: 'danger', INIT: 'warn', STOP: '' }

export default function VRRP() {
  const status = usePoll('/api/network/vrrp', { every: 3000 })
  const st = status.data
  if (st?.state === 'not_enabled') return <Module module="vrrp" />
  return (
    <>
      <PageHeader
        title="VRRP · keepalived"
        subtitle="Virtual IPs shared by several nodes, moved to another one when this one fails - or when its HAProxy stops answering"
        actions={
          <a className="button small" href={DOCS} target="_blank" rel="noreferrer">
            VRRP guide <ExternalLink size={12} />
          </a>
        }
      />
      {status.error && <ErrorBox error={status.error} />}
      <Card
        title="Instances"
        icon={Boxes}
        actions={
          st && (
            <Badge tone={st.haproxy_healthy ? 'ok' : 'danger'} dot>
              HAProxy {st.haproxy_healthy ? 'answers' : 'not answering'}
            </Badge>
          )
        }
      >
        {!st ? (
          <Loading />
        ) : !st.configured ? (
          <span className="muted small">No keepalived.conf is saved: keepalived doesn't run. Write one below.</span>
        ) : st.error ? (
          <div className="error-box">{st.error}</div>
        ) : !st.instances.length ? (
          <span className="muted small">keepalived is starting…</span>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Instance</th>
                  <th>State</th>
                  <th>Interface</th>
                  <th>VRID</th>
                  <th>Priority</th>
                  <th>Virtual IPs</th>
                  <th>Since</th>
                </tr>
              </thead>
              <tbody>
                {st.instances.map((i) => (
                  <tr key={i.name}>
                    <td className="mono">{i.name}</td>
                    <td>
                      <Badge tone={TONES[i.state]} dot>
                        {i.state}
                      </Badge>
                    </td>
                    <td className="mono">{i.interface}</td>
                    <td className="mono">{i.vrid}</td>
                    <td className="mono" title="effective / configured">
                      {i.effective_priority}/{i.priority}
                    </td>
                    <td className="mono">{(i.virtual_ips || []).join(' ')}</td>
                    <td className="small">{i.last_transition_unix ? new Date(i.last_transition_unix * 1000).toLocaleString() : '–'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {st && (
          <p className="small muted" style={{ marginBottom: 0 }}>
            Track <span className="mono">{st.haproxy_health_file}</span> (track_file, weight 0) to give the virtual IPs up when this node's HAProxy stops answering. With the
            firewall extension, accept VRRP: <span className="mono">ip protocol 112 accept</span>.
          </p>
        )}
      </Card>
      <div style={{ marginTop: '1rem' }}>
        <ModuleConfigEditor
          base="/api/network/vrrp"
          applyMethod="NetworkService/VRRPApplyConfig"
          readMethod="NetworkService/VRRPGetConfig"
          file="keepalived.conf"
          daemon="keepalived"
          starter={STARTER}
          applyNote="keepalived checks it, then reloads it: instances whose state doesn't change keep their virtual IPs."
          onApplied={status.reload}
        />
      </div>
    </>
  )
}
