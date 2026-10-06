import { CheckCircle2, Download, ExternalLink, ListPlus, RotateCcw, Shield, ShieldOff, Trash2, Upload } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { getJSON, postJSON } from '../api.js'
import { Badge, Card, ErrorBox, Loading, PageHeader, Tabs, useAction, useConfirm, useToast } from '../../shared/ui.jsx'
import { DiffView, Editor } from '../components/Editor.jsx'
import { hunks, lineDiff } from '../diff.js'
import { usePoll } from '../hooks.jsx'
import { useMay } from '../may.js'
import Module from './Module.jsx'

// Its page on the docs site, by the file's permalink.
const DOCS = 'https://janus.sw-servers.net/docs/firewall.md'

// A starting point for a node with no ruleset yet: checked against the
// extension's own nft.
const STARTER = `# The node's whole nftables ruleset - see docs/firewall.md.
# Applied on trial: if it cuts the Controller off, the node reverts by itself.
table inet filter {
\t# Addresses to drop - editable live, from the Sets tab or the API.
\tset blocklist {
\t\ttype ipv4_addr
\t\tflags interval, timeout
\t}

\tchain input {
\t\ttype filter hook input priority filter; policy drop;
\t\tct state established,related accept
\t\tct state invalid drop
\t\tiif "lo" accept
\t\tip saddr @blocklist drop
\t\ticmp type echo-request limit rate 10/second accept
\t\ticmpv6 type { nd-neighbor-solicit, nd-neighbor-advert, nd-router-advert, echo-request } accept
\t\ttcp dport 9505 accept comment "Janus API"
\t\ttcp dport 10056 accept comment "Janus exporter"
\t\ttcp dport { 80, 443 } accept comment "HAProxy - your frontends' ports"
\t}
}
`

function useCountdown(unix) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!unix) return undefined
    const t = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(t)
  }, [unix])
  return unix ? Math.max(0, Math.round(unix - now / 1000)) : 0
}

export default function Firewall() {
  const status = usePoll('/api/network/firewall', { every: 5000 })
  const [tab, setTab] = useState('ruleset')
  const st = status.data
  const left = useCountdown(st?.trial_pending ? st.trial_revert_at_unix : 0)
  const [busy, run] = useAction()
  const may = useMay()
  const shown = (tab === 'ruleset' && !may('NetworkService/FirewallGetRuleset')) || (tab === 'sets' && !may('NetworkService/FirewallSets')) ? 'live' : tab

  if (st?.state === 'not_enabled' || (status.error && /isn't in this node's image/.test(String(status.error.message)))) return <Module module="firewall" />

  let badge = <Badge>…</Badge>
  if (st?.trial_pending) badge = <Badge tone="warn" dot>On trial - reverts in {left}s</Badge>
  else if (st?.configured) badge = <Badge tone="ok" dot>Active</Badge>
  else if (st) badge = <Badge>No firewall</Badge>

  return (
    <>
      <PageHeader
        title="Firewall · nftables"
        subtitle="The node's nftables ruleset - checked before it's applied, applied on trial, reverted by itself if it cuts the node off"
        actions={
          <a className="button small" href={DOCS} target="_blank" rel="noreferrer">
            Firewall guide <ExternalLink size={12} />
          </a>
        }
      />
      {status.error && <ErrorBox error={status.error} />}
      <Card title="State" icon={Shield} actions={badge}>
        {st?.trial_pending ? (
          <div className="row">
            <span className="small">A ruleset is on trial. Unless it's confirmed over a new connection, the node puts the previous one back in {left}s.</span>
            <span className="grow" />
            {may('NetworkService/FirewallConfirm') && (
              <button
                className="primary small"
                disabled={busy}
                onClick={() =>
                  run(async () => {
                    await postJSON('/api/network/firewall/confirm', {})
                    status.reload()
                  }, 'Ruleset confirmed and saved')
                }
              >
                Confirm now
              </button>
            )}
          </div>
        ) : st?.configured ? (
          <span className="small">The saved ruleset is applied - at every boot too.</span>
        ) : (
          <span className="small muted">No ruleset is saved: the node doesn't filter anything. Write one in the editor below.</span>
        )}
      </Card>
      <div style={{ marginTop: '1rem' }}>
        <Tabs
          tabs={[
            // The saved ruleset and the sets are the network domain's; the
            // live ruleset is the node's state.
            ...(may('NetworkService/FirewallGetRuleset') ? [{ id: 'ruleset', label: 'Ruleset' }] : []),
            ...(may('NetworkService/FirewallSets') ? [{ id: 'sets', label: 'Sets' }] : []),
            { id: 'live', label: 'Live ruleset' },
          ]}
          active={shown}
          onChange={setTab}
        />
        {shown === 'ruleset' && <RulesetEditor onApplied={status.reload} />}
        {shown === 'sets' && <Sets />}
        {shown === 'live' && (
          <Card title="What the kernel has now">
            {st ? <pre className="code" style={{ maxHeight: '70vh' }}>{st.ruleset || '(empty ruleset)'}</pre> : <Loading />}
          </Card>
        )}
      </div>
    </>
  )
}

function RulesetEditor({ onApplied }) {
  const [saved, setSaved] = useState(null) // { ruleset, is_default }
  const [draft, setDraft] = useState('')
  const [loadError, setLoadError] = useState(null)
  const [check, setCheck] = useState(null)
  const [timeout, setTimeoutS] = useState('30')
  const [result, setResult] = useState(null)
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const canApply = useMay()('NetworkService/FirewallApplyRuleset')
  const toast = useToast()

  const load = async () => {
    try {
      const r = await getJSON('/api/network/firewall/ruleset')
      setSaved(r)
      setDraft(r.is_default ? STARTER : r.ruleset)
      setCheck(null)
      setLoadError(null)
    } catch (err) {
      setLoadError(err)
    }
  }
  useEffect(() => {
    load()
  }, [])

  const original = saved ? saved.ruleset : ''
  const dirty = saved != null && draft !== original
  const diff = useMemo(() => (dirty ? hunks(lineDiff(original, draft)) : []), [dirty, original, draft])

  const doCheck = () =>
    run(async () => {
      setCheck(await postJSON('/api/network/firewall/check', { ruleset: draft }))
    })

  const apply = async (ruleset, title) => {
    const ok = await confirm({
      title,
      body: (
        <>
          <p>
            The node checks the ruleset, then applies it <strong>on trial</strong>. The Controller confirms it over a new connection; if it can't reach the node any more, the node puts
            the previous ruleset back by itself after {Number(timeout) || 30} seconds.
          </p>
          {ruleset ? <DiffView diff={hunks(lineDiff(original, ruleset))} /> : <p>The node will no longer filter anything.</p>}
        </>
      ),
      action: ruleset ? 'Apply on trial' : 'Remove the firewall',
      danger: !ruleset,
      wide: true,
    })
    if (!ok) return
    const r = await run(() => postJSON('/api/network/firewall/apply', { ruleset, confirm_timeout_seconds: Number(timeout) || 30 }))
    if (!r) return
    setResult(r)
    if (!r.accepted) {
      setCheck({ accepted: false, errors: r.errors })
      return
    }
    if (r.confirmed) {
      toast(ruleset ? 'Ruleset applied, confirmed and saved' : 'Firewall removed')
      await load()
    } else {
      toast('Not confirmed - the node reverts by itself', 'warn')
    }
    onApplied()
  }

  const download = () => {
    const a = document.createElement('a')
    a.href = URL.createObjectURL(new Blob([draft], { type: 'text/plain' }))
    a.download = 'firewall.nft'
    a.click()
  }
  const importFile = (e) => {
    const f = e.target.files[0]
    if (f) f.text().then(setDraft)
    e.target.value = ''
  }

  if (loadError) return <ErrorBox error={loadError} />
  if (!saved) return <Loading />
  return (
    <div className="stack">
      {result && !result.confirmed && result.accepted && (
        <div className="notice">
          <strong>Not confirmed.</strong> {result.error} The node reverts at {new Date(result.revert_at_unix * 1000).toLocaleTimeString()}.
        </div>
      )}
      <Card
        title="firewall.nft"
        actions={
          saved.is_default ? (
            <Badge>a starting point - nothing saved yet</Badge>
          ) : dirty ? (
            <Badge tone="warn">{diff.filter((d) => d.op === '+' || d.op === '-').length} changed lines</Badge>
          ) : (
            <Badge tone="ok">matches the saved ruleset</Badge>
          )
        }
      >
        <p className="small muted" style={{ marginTop: 0 }}>
          The node's whole ruleset, in nft's syntax: whatever it doesn't list is removed. Write protocols that musl doesn't name by number (VRRP is <span className="mono">ip protocol 112</span>).
        </p>
        <Editor
          value={draft}
          onChange={(v) => {
            setDraft(v)
            setCheck(null)
          }}
        />
        <div className="row" style={{ marginTop: '0.8rem', flexWrap: 'wrap' }}>
          {canApply && (
            <>
              <button disabled={busy} onClick={doCheck}>
                <CheckCircle2 size={15} /> Check
              </button>
              <button className="primary" disabled={busy || (!dirty && !saved.is_default) || !draft.trim()} onClick={() => apply(draft, 'Apply this ruleset?')}>
                <Upload size={15} /> Apply…
              </button>
              <label className="field" style={{ width: '10rem' }} title="How long the node waits for the confirmation before reverting">
                <input className="mono" aria-label="Seconds to confirm" inputMode="numeric" value={timeout} onChange={(e) => setTimeoutS(e.target.value.replace(/\D/g, ''))} placeholder="30" />
              </label>
              <span className="small muted">s to confirm</span>
            </>
          )}
          <button disabled={busy || !dirty} onClick={() => setDraft(original || STARTER)}>
            <RotateCcw size={15} /> Revert
          </button>
          <span className="grow" />
          {!saved.is_default && canApply && (
            <button className="danger small" disabled={busy} onClick={() => apply('', 'Remove the firewall?')}>
              <ShieldOff size={14} /> Remove the firewall…
            </button>
          )}
          <label className="button small">
            Import file
            <input type="file" hidden onChange={importFile} />
          </label>
          <button className="small" onClick={download}>
            <Download size={14} /> Download
          </button>
        </div>
        {check && (
          <div style={{ marginTop: '0.8rem' }}>
            {check.accepted ? <div className="notice">Valid: nft accepts this ruleset.</div> : <div className="error-box">{(check.errors || ['refused']).join('\n')}</div>}
          </div>
        )}
      </Card>
      {dirty && !saved.is_default && (
        <Card title="Changes against the saved ruleset">
          <DiffView diff={diff} />
        </Card>
      )}
    </div>
  )
}

function fmtDuration(s) {
  if (s >= 86400) return `${Math.round(s / 86400)}d`
  if (s >= 3600) return `${Math.round(s / 3600)}h`
  if (s >= 60) return `${Math.round(s / 60)}m`
  return `${s}s`
}

function Sets() {
  const { data, error, reload } = usePoll('/api/network/firewall/sets', { every: 10000 })
  if (error) return <ErrorBox error={error} />
  if (!data) return <Loading />
  const sets = data.sets || []
  if (!sets.length)
    return (
      <Card>
        <span className="muted">The live ruleset has no named set. Declare one in the ruleset (like the starting point's blocklist) to edit its elements here.</span>
      </Card>
    )
  return (
    <div className="stack">
      {sets.map((s) => (
        <SetCard key={`${s.family} ${s.table} ${s.name}`} set={s} reload={reload} />
      ))}
    </div>
  )
}

function SetCard({ set, reload }) {
  const [values, setValues] = useState('')
  const [ttl, setTtl] = useState('')
  const [busy, run] = useAction()
  const confirm = useConfirm()
  const elements = set.elements || []
  const canTimeout = (set.flags || []).includes('timeout')
  const canUpdate = useMay()('NetworkService/FirewallSetUpdate')
  const update = (body, msg) =>
    run(async () => {
      await postJSON('/api/network/firewall/sets', { family: set.family, table: set.table, set: set.name, ...body })
      reload()
      return true
    }, msg)

  const add = () => {
    const list = values
      .split(/[,\n]/)
      .map((v) => v.trim())
      .filter(Boolean)
    if (!list.length) return
    const timeout_seconds = Number(ttl) * 60 || 0
    // Cleared at once, so typing the next ones while this is sent isn't
    // lost; put back if the node refuses them.
    setValues('')
    update({ add: list.map((value) => ({ value, timeout_seconds })) }, `Added ${list.length} element${list.length > 1 ? 's' : ''}`).then((ok) => {
      if (!ok) setValues(list.join(', '))
    })
  }
  const remove = async (value) => {
    if (await confirm({ title: `Remove ${value}?`, body: <p>From {set.family} {set.table} {set.name}, at once.</p>, action: 'Remove', danger: true })) {
      update({ delete: [value] }, `Removed ${value}`)
    }
  }

  return (
    <Card
      title={`${set.family} ${set.table} ${set.name}`}
      icon={ListPlus}
      actions={
        <span className="small muted mono">
          {set.type}
          {set.flags?.length ? ` · ${set.flags.join(', ')}` : ''} · {elements.length} elements
        </span>
      }
    >
      {canUpdate && (
      <div className="row" style={{ alignItems: 'flex-end', flexWrap: 'wrap', marginBottom: '0.8rem' }}>
        <label className="field grow">
          <span>Add elements (comma-separated, nft syntax: 192.0.2.7, 198.51.100.0/24, 10.0.0.1-10.0.0.9)</span>
          <input className="mono" aria-label="Elements to add" value={values} onChange={(e) => setValues(e.target.value)} onKeyDown={(e) => e.key === 'Enter' && add()} />
        </label>
        {canTimeout && (
          <label className="field" style={{ width: '9rem' }}>
            <span>Expire after (min)</span>
            <input className="mono" aria-label="Expire after (minutes)" inputMode="numeric" placeholder="never" value={ttl} onChange={(e) => setTtl(e.target.value.replace(/\D/g, ''))} />
          </label>
        )}
        <button className="primary" disabled={busy || !values.trim()} onClick={add}>
          Add
        </button>
      </div>
      )}
      <p className="small muted" style={{ marginTop: 0 }}>
        Elements added without an expiry are kept: on the node, they come back after every apply and reboot. Elements with one expire, and don't survive re-applying the ruleset.
      </p>
      {elements.length ? (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Element</th>
                <th>Kept</th>
                <th>Expires in</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {elements.map((e) => (
                <tr key={e.value}>
                  <td className="mono">{e.value}</td>
                  <td>{e.persistent ? <Badge tone="ok">kept</Badge> : <span className="muted small">{e.timeout_seconds ? 'expires' : 'from the ruleset'}</span>}</td>
                  <td className="mono small">{e.timeout_seconds ? fmtDuration(e.expires_seconds || 0) : '–'}</td>
                  <td style={{ textAlign: 'right' }}>
                    {canUpdate && (
                      <button className="small danger" disabled={busy} onClick={() => remove(e.value)} title="Remove">
                        <Trash2 size={13} />
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <span className="muted small">Empty.</span>
      )}
    </Card>
  )
}
