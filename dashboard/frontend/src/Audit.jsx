import { Filter, RefreshCw, ScrollText, Search, X } from 'lucide-react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { filtersQuery, filtersRoute, fromLocalInput, KINDS, readFilters, RESULTS, SUBJECT_KINDS, subjectGroups, subjectHref, subjectLabel, toLocalInput, viaLabel } from './audit.js'
import { call } from './call.js'
import { navigate, useHashRoute } from './shared/route.js'
import { Badge, Card, ErrorBox } from './shared/ui.jsx'
import { fmtDateTime } from './shared/units.js'

// The audit (admin): every change made on this Controller and every
// sign-in, newest first, a page at a time - dashboard/backend/internal/
// audit, GET /api/audit. The filters are the hash's (audit.js): a
// filtered view can be shared; the pickers' choices are what the audit
// names (?facets=1), a removed node too.

const PAGE = 100

function statusTone(code) {
  if (code >= 500) return 'danger'
  if (code >= 400) return 'warn'
  return 'ok'
}

// Subject names what an entry is about - as it was named then -, a link
// to it while it exists; onOnly keeps only the entries about it.
function Subject({ s, onOnly }) {
  const href = subjectHref(s)
  const label = subjectLabel(s)
  const kind = SUBJECT_KINDS[s.kind] || s.kind
  return (
    <span className="audit-subject">
      <span>{kind}</span>
      {href ? (
        <a href={href} title={s.id} {...(s.kind === 'node' ? { target: '_blank', rel: 'noopener' } : {})}>
          <strong>{label}</strong>
        </a>
      ) : (
        <strong title={s.id}>{label}</strong>
      )}
      {s.gone && <span className="muted small">(removed)</span>}
      {onOnly && (
        <button type="button" className="ghost icon" title={`Only the entries about ${label}`} aria-label={`Only the entries about ${kind} ${label}`} onClick={onOnly}>
          <Filter size={13} />
        </button>
      )}
    </span>
  )
}

// The fields typed in, as the hash has them.
function typed(f) {
  return { from: toLocalInput(f.from), to: toLocalInput(f.to), client: f.client || '', q: f.q || '' }
}

// Filters: the pickers apply at once, the fields typed in on Enter (or
// Apply) or when left - and follow the hash (a link, Back).
function Filters({ filters, facets, set }) {
  const [text, setText] = useState(() => typed(filters))
  const [shown, setShown] = useState(filters)
  if (shown !== filters) {
    // Only the fields whose filter changed: one applied as it's left
    // leaves the next one being typed in alone.
    const now = typed(filters)
    const before = typed(shown)
    setShown(filters)
    setText((t) => Object.fromEntries(Object.entries(t).map(([k, v]) => [k, now[k] !== before[k] ? now[k] : v])))
  }
  const apply = (e) => {
    e?.preventDefault()
    set({ from: fromLocalInput(text.from), to: fromLocalInput(text.to), client: text.client.trim(), q: text.q.trim() })
  }
  const edit = (k) => (e) => setText((t) => ({ ...t, [k]: e.target.value }))
  const users = facets?.users || []
  const vias = (facets?.vias || []).filter((v) => v !== 'session')
  const tokens = vias.filter((v) => v.startsWith('token '))
  const others = vias.filter((v) => !v.startsWith('token '))
  const keep = (list, v) => (v && !list.includes(v) ? [...list, v] : list)
  const methods = keep(facets?.methods || [], filters.method)
  const pick = (k, label, options) => (
    <label className="field">
      <span>{label}</span>
      <select value={filters[k] || ''} onChange={(e) => set({ [k]: e.target.value })}>
        {options}
      </select>
    </label>
  )
  return (
    <form className="audit-filters" onSubmit={apply}>
      {pick(
        'user',
        'Account',
        <>
          <option value="">Any</option>
          {keep(users, filters.user).map((u) => (
            <option key={u} value={u}>
              {u}
            </option>
          ))}
        </>,
      )}
      {pick(
        'via',
        'How',
        <>
          <option value="">Any way</option>
          <option value="session">Session</option>
          <option value="token">Any API token</option>
          {keep(tokens, filters.via?.startsWith('token ') ? filters.via : '').map((v) => (
            <option key={v} value={v}>
              {viaLabel(v)}
            </option>
          ))}
          <option value="none">No credential</option>
          {keep(others, filters.via && !['session', 'token', 'none'].includes(filters.via) && !filters.via.startsWith('token ') ? filters.via : '').map((v) => (
            <option key={v} value={v}>
              {viaLabel(v)}
            </option>
          ))}
        </>,
      )}
      {pick(
        'subject',
        'About',
        <>
          <option value="">Anything</option>
          {subjectGroups(facets?.subjects, filters.subject).map((g) => (
            <optgroup key={g.kind} label={g.label}>
              {g.items.map((s) => (
                <option key={`${s.kind}:${s.id}`} value={`${s.kind}:${s.id}`}>
                  {subjectLabel(s)}
                  {s.gone ? ' (removed)' : ''}
                </option>
              ))}
            </optgroup>
          ))}
        </>,
      )}
      {pick(
        'kind',
        'Kind of action',
        <>
          <option value="">Any</option>
          {KINDS.map(([id, label]) => (
            <option key={id} value={id}>
              {label}
            </option>
          ))}
        </>,
      )}
      {pick(
        'result',
        'Result',
        <>
          <option value="">Any</option>
          {RESULTS.map(([id, label]) => (
            <option key={id} value={id}>
              {label}
            </option>
          ))}
        </>,
      )}
      {pick(
        'method',
        'Method',
        <>
          <option value="">Any</option>
          {methods.map((m) => (
            <option key={m} value={m}>
              {m}
            </option>
          ))}
        </>,
      )}
      <label className="field audit-wide">
        <span>From</span>
        <input type="datetime-local" value={text.from} onChange={edit('from')} onBlur={apply} />
      </label>
      <label className="field audit-wide">
        <span>Until</span>
        <input type="datetime-local" value={text.to} onChange={edit('to')} onBlur={apply} />
      </label>
      <label className="field audit-wide">
        <span>Client address</span>
        <input value={text.client} onChange={edit('client')} onBlur={apply} placeholder="10.1.2." className="mono" />
      </label>
      <label className="field audit-text">
        <span>Path or name contains</span>
        <input type="search" value={text.q} onChange={edit('q')} onBlur={apply} placeholder="haproxy/config" />
      </label>
      <div className="audit-apply">
        <button type="submit" className="ghost" title="Apply the typed filters (Enter)">
          <Search size={15} /> Apply
        </button>
      </div>
    </form>
  )
}

export default function AuditPage() {
  const route = useHashRoute()
  const filters = useMemo(() => readFilters(route), [route])
  const query = filtersQuery(filters)
  const active = query !== ''
  const [facets, setFacets] = useState(null)
  const [page, setPage] = useState(null) // { entries, total, next }
  const [error, setError] = useState(null)
  const [busy, setBusy] = useState(false)
  // The query a page in flight was asked for: one the filters changed
  // under isn't added.
  const current = useRef(query)
  useEffect(() => {
    current.current = query
  }, [query])

  const set = useCallback((patch) => navigate(filtersRoute({ ...filters, ...patch })), [filters])

  const fetchPage = useCallback(
    (before) => {
      const q = new URLSearchParams(query)
      q.set('limit', String(PAGE))
      if (before) q.set('before', before)
      return call(`/api/audit?${q}`, { withHeaders: true }).then(({ body, headers }) => ({
        query,
        entries: body || [],
        total: Number(headers.get('X-Total-Count') || 0),
        next: headers.get('X-Next-Before') || '',
      }))
    },
    [query],
  )

  const load = useCallback(() => {
    fetchPage('').then((p) => {
      if (p.query !== current.current) return
      setPage(p)
      setError(null)
    }, setError)
  }, [fetchPage])
  useEffect(load, [load])

  const loadFacets = useCallback(() => {
    call('/api/audit?facets=1').then(setFacets, setError)
  }, [])
  useEffect(loadFacets, [loadFacets])

  const older = () => {
    setBusy(true)
    fetchPage(page.next)
      .then((p) => {
        if (p.query !== current.current) return
        setPage((cur) => ({ ...p, entries: [...cur.entries, ...p.entries] }))
      }, setError)
      .finally(() => setBusy(false))
  }

  const entries = page?.query === query ? page.entries : null
  return (
    <div className="stack">
      <div className="spread">
        <h1>Audit</h1>
      </div>
      <p className="muted" style={{ margin: 0 }}>
        Every change made on this Controller - from its pages or with an API token - and every sign-in, with who made it and what it was about, named as it was then. What the
        Controller does on a node, the node logs too, with the same name.
      </p>
      <Card
        title="Filters"
        icon={Filter}
        actions={
          active && (
            <button className="ghost" type="button" onClick={() => navigate('/audit')}>
              <X size={15} /> Clear
            </button>
          )
        }
      >
        <Filters filters={filters} facets={facets} set={set} />
      </Card>
      <Card
        title="Entries"
        icon={ScrollText}
        actions={
          <div className="row">
            {entries && (
              <span className="muted small" aria-live="polite">
                {page.total.toLocaleString()} {active ? 'matching' : 'kept'}
              </span>
            )}
            <button
              className="ghost icon"
              type="button"
              title="Refresh"
              aria-label="Refresh"
              disabled={busy}
              onClick={() => {
                load()
                loadFacets()
              }}
            >
              <RefreshCw size={15} />
            </button>
          </div>
        }
      >
        <ErrorBox error={error} />
        {entries && entries.length === 0 && <div className="muted">{active ? 'Nothing matches these filters.' : 'Nothing yet.'}</div>}
        {entries && entries.length > 0 && (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>When</th>
                  <th>Who</th>
                  <th>What</th>
                  <th>Result</th>
                  <th>From</th>
                </tr>
              </thead>
              <tbody>
                {entries.map((e, i) => (
                  <tr key={e.id ? e.id : `p${i}`}>
                    <td style={{ whiteSpace: 'nowrap' }}>{fmtDateTime(e.time)}</td>
                    <td>
                      {e.user ? <strong>{e.user}</strong> : <span className="muted">–</span>} {e.via && e.via !== 'session' && <span className="muted small mono">{e.via}</span>}
                    </td>
                    <td>
                      {e.subject ? (
                        <>
                          <div className="row" style={{ gap: '0.4rem' }}>
                            <span className="mono small">{e.method}</span>
                            <Subject s={e.subject} onOnly={filters.subject === `${e.subject.kind}:${e.subject.id}` ? null : () => set({ subject: `${e.subject.kind}:${e.subject.id}` })} />
                          </div>
                          <div className="muted small mono audit-path">{e.path}</div>
                        </>
                      ) : (
                        <span className="mono small audit-path">
                          {e.method} {e.path}
                        </span>
                      )}
                    </td>
                    <td>
                      <Badge tone={statusTone(e.status)}>{e.status}</Badge>
                    </td>
                    <td className="mono small">{e.client}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {entries && entries.length > 0 && (
          <div className="spread" style={{ marginTop: '0.8rem' }}>
            <span className="muted small">
              {entries.length.toLocaleString()} of {page.total.toLocaleString()} shown
            </span>
            {page.next && (
              <button type="button" onClick={older} disabled={busy}>
                Load older
              </button>
            )}
          </div>
        )}
      </Card>
    </div>
  )
}
