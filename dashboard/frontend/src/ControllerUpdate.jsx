import { ArrowUpRight, CheckCircle2, Copy, Loader2, Rocket, TriangleAlert, X } from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'
import { Card, useConfirm, useToast } from './shared/ui.jsx'
import { backgroundHeaders } from './call.js'
import { useCan } from './me.jsx'
import { SecurityBadge } from './SecurityBadge.jsx'
import { securityText, securityTone } from './severity.js'

// The Controller's own updates (dashboard/README.md, "Updating the
// Controller"): GET /api/controller/update says whether a newer release
// exists and whether janus-controller-updater runs next to this
// Controller; POST asks it to install the newest release. The Controller
// restarts during an update - sessions don't survive that, so the page
// reloads into the sign-in screen once the new one answers, and shows how
// the update ended after signing in.

const DOCS = 'https://github.com/swenske/Janus/blob/main/dashboard/README.md#updating-the-controller'
const DISMISSED = 'janus.controllerUpdate.dismissed'
const IDLE_EVERY = 60000
const BUSY_EVERY = 2000

function storedDismissed() {
  try {
    return localStorage.getItem(DISMISSED) || ''
  } catch {
    return ''
  }
}

const RESULTS = {
  done: { tone: 'ok', title: (j) => `The Controller was updated to ${j.to_version}` },
  'rolled-back': { tone: 'danger', title: (j) => `The update to ${j.to_version} was rolled back` },
  failed: { tone: 'danger', title: (j) => `The update to ${j.to_version} didn't start` },
  'rollback-failed': { tone: 'danger', title: (j) => `The update to ${j.to_version} failed, and so did its rollback` },
  interrupted: { tone: 'warn', title: (j) => `The update to ${j.to_version} was interrupted` },
}

function JobLog({ job }) {
  if (!job.log?.length) return null
  return (
    <details>
      <summary className="small muted">Updater log</summary>
      <pre className="update-log">{job.log.join('\n')}</pre>
    </details>
  )
}

export default function ControllerUpdate() {
  const can = useCan()
  const [info, setInfo] = useState(null)
  // 'starting' from the click until the updater reports the job;
  // 'restarting' while the Controller doesn't answer.
  const [phase, setPhase] = useState(null)
  const [dismissed, setDismissed] = useState(storedDismissed)
  const [manualOpen, setManualOpen] = useState(false)
  const envRef = useRef(null)
  const busyRef = useRef(false)
  const confirm = useConfirm()
  const toast = useToast()

  const job = info?.updater?.job
  const running = phase !== null || job?.state === 'running'

  const load = useCallback(async () => {
    // During an update, ask whether this session still exists first: the
    // new Controller doesn't know it, and the page has to sign in again.
    if (busyRef.current) {
      try {
        const auth = await fetch('/api/auth/status', { headers: backgroundHeaders() })
        if (!auth.ok) return setPhase((p) => p || 'restarting')
        if (!(await auth.json()).authenticated) return window.location.reload()
      } catch {
        return setPhase((p) => p || 'restarting')
      }
    }
    let resp
    try {
      resp = await fetch('/api/controller/update', { headers: backgroundHeaders() })
    } catch {
      setPhase((p) => (p ? 'restarting' : p))
      return
    }
    // A new Controller: the session is gone with the old one.
    if (resp.status === 401) return window.location.reload()
    if (!resp.ok) {
      setPhase((p) => (p ? 'restarting' : p))
      return
    }
    const next = await resp.json()
    setInfo({ ...next, fetchedAt: Date.now() })
    setPhase((p) => (p && next.updater?.job?.state === 'running' ? null : p))
  }, [])

  useEffect(() => {
    busyRef.current = running
    load()
    const t = setInterval(load, running ? BUSY_EVERY : IDLE_EVERY)
    return () => clearInterval(t)
  }, [load, running])

  if (!info) return null
  const latest = info.latest
  const updater = info.updater || {}

  const start = async () => {
    const ok = await confirm({
      title: `Update the Controller to ${latest.version}?`,
      body: (
        <>
          <p style={{ margin: 0 }}>
            The updater pulls{' '}
            <span className="mono small" style={{ wordBreak: 'break-all' }}>
              {latest.image}
            </span>
            , backs up the Controller&apos;s data and <code>.env</code>, then restarts the Controller on the new version.
          </p>
          <ul style={{ margin: 0, paddingLeft: '1.2rem' }}>
            <li>This page reconnects by itself and asks you to sign in again. Open node pages reconnect too.</li>
            <li>Nodes keep running; only the Controller restarts.</li>
            <li>If {latest.version} doesn&apos;t start, the previous version comes back with its data, automatically.</li>
          </ul>
        </>
      ),
      action: `Update to ${latest.version}`,
    })
    if (!ok) return
    setPhase('starting')
    try {
      const resp = await fetch('/api/controller/update', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ version: latest.version }),
      })
      if (!resp.ok) throw new Error((await resp.text()).trim() || resp.statusText)
      load()
    } catch (err) {
      setPhase(null)
      toast(err, 'danger')
    }
  }

  // --- an update running ---
  if (running) {
    const to = job?.state === 'running' ? job.to_version : latest?.version
    return (
      <Card title={`Updating the Controller to ${to}`} icon={Rocket} className="controller-update accent">
        <div className="row">
          <Loader2 size={15} className="spin" />
          <span>{phase === 'restarting' ? 'The Controller is restarting…' : job?.state === 'running' ? job.message : 'Starting the update…'}</span>
        </div>
        <p className="muted small" style={{ margin: 0 }}>
          The Controller restarts during the update: this page reconnects by itself, then asks you to sign in again.
        </p>
      </Card>
    )
  }

  const blocks = []

  // --- how the last update ended ---
  const result = job && RESULTS[job.state]
  const recent = job?.finished_at && info.fetchedAt - new Date(job.finished_at).getTime() < 7 * 24 * 3600 * 1000
  if (result && recent && dismissed !== job.id) {
    const dismiss = () => {
      setDismissed(job.id)
      try {
        localStorage.setItem(DISMISSED, job.id)
      } catch {
        // per-browser convenience only
      }
    }
    blocks.push(
      <Card
        key="result"
        title={result.title(job)}
        icon={result.tone === 'ok' ? CheckCircle2 : TriangleAlert}
        className={`controller-update ${result.tone}`}
        actions={
          <button className="ghost icon" onClick={dismiss} aria-label="Dismiss">
            <X size={16} />
          </button>
        }
      >
        {job.state !== 'done' && <p style={{ margin: 0 }}>{job.message}</p>}
        <div className="muted small">
          {job.from_version} → {job.to_version} · {new Date(job.finished_at).toLocaleString()}
        </div>
        {job.state === 'rollback-failed' && (
          <div className="notice warn">
            The Controller may not be running. The data and <code>.env</code> from before the update are in the updater&apos;s state volume -{' '}
            <a href={`${DOCS.replace('#updating-the-controller', '#when-an-update-fails')}`} target="_blank" rel="noreferrer">
              what to do
            </a>
            .
          </div>
        )}
        <JobLog job={job} />
      </Card>,
    )
  }

  // --- a newer release ---
  if (info.update_available && latest) {
    const envLine = `JANUS_CONTROLLER_IMAGE=${latest.image}`
    const copyEnv = async () => {
      try {
        await navigator.clipboard.writeText(envLine)
        toast('Copied')
      } catch {
        envRef.current?.select()
        toast('Select the line and press Ctrl+C', 'warn')
      }
    }
    blocks.push(
      <Card
        key="available"
        title={`Janus Controller ${latest.version} is available`}
        icon={Rocket}
        className="controller-update accent"
        actions={
          <a className="small row" style={{ gap: '0.2rem' }} href={latest.url} target="_blank" rel="noreferrer">
            Release notes <ArrowUpRight size={14} />
          </a>
        }
      >
        {info.security_update && (
          <div className={`notice small ${securityTone(info.security_update)}`}>
            <SecurityBadge severity={info.security_update} /> {securityText(info.security_update)}
          </div>
        )}
        <div className="muted small">
          {info.version_known ? (
            <>
              This Controller runs <span className="mono">{info.version}</span>
            </>
          ) : (
            <>
              This Controller is a development build (<span className="mono">{info.version}</span>)
            </>
          )}
          {latest.published_at && (
            <>
              {' '}
              · {latest.version} published {new Date(latest.published_at).toLocaleDateString()}
            </>
          )}
        </div>
        {!can('admin') ? (
          <div className="muted small">An admin can update it from here.</div>
        ) : updater.reachable && updater.ready ? (
          <div className="row">
            <button className="primary" onClick={start}>
              <Rocket size={15} /> Update to {latest.version}
            </button>
            <span className="muted small">
              Compose project <span className="mono">{updater.project}</span>, service <span className="mono">{updater.service}</span>
            </span>
          </div>
        ) : (
          <>
            {updater.configured && (
              <div className="notice warn small">
                <strong>The updater can&apos;t update this Controller yet</strong>
                {updater.reachable ? (
                  <ul style={{ margin: '0.3rem 0 0', paddingLeft: '1.2rem' }}>
                    {(updater.problems || []).map((p) => (
                      <li key={p}>{p}</li>
                    ))}
                  </ul>
                ) : (
                  <div>
                    It doesn&apos;t answer - is the <span className="mono">janus-controller-updater</span> container running? <span className="muted">({updater.error})</span>
                  </div>
                )}
              </div>
            )}
            <div className="small">
              {updater.configured ? 'Until then, update' : 'Update'} by hand in the directory of your <code>compose.yaml</code>, or{' '}
              <a href={DOCS} target="_blank" rel="noreferrer">
                {updater.configured ? 'see the setup' : 'set up one-click updates'}
              </a>
              .{' '}
              <button className="small ghost" onClick={() => setManualOpen(!manualOpen)}>
                {manualOpen ? 'Hide' : 'How'}
              </button>
            </div>
            {manualOpen && (
              <div className="stack" style={{ gap: '0.4rem' }}>
                <label className="field">
                  <span className="spread">
                    <span>
                      In <code>.env</code>, next to <code>compose.yaml</code>:
                    </span>
                    <button type="button" className="small ghost" onClick={copyEnv}>
                      <Copy size={13} /> Copy
                    </button>
                  </span>
                  <input ref={envRef} readOnly className="mono" value={envLine} />
                </label>
                <div className="small">
                  then <code>docker compose up -d</code> - with the compose file from the{' '}
                  <a href={DOCS} target="_blank" rel="noreferrer">
                    documentation
                  </a>
                  , whose image comes from <code>JANUS_CONTROLLER_IMAGE</code>.
                </div>
              </div>
            )}
          </>
        )}
      </Card>,
    )
  }

  if (!blocks.length) return null
  return <>{blocks}</>
}
