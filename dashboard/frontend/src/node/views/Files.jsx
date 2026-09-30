import { ArrowUp, Download, File, FileArchive, Folder, FolderOpen, Link2 } from 'lucide-react'
import { useEffect, useState } from 'react'
import { download, getJSON, getText } from '../api.js'
import { Card, ErrorBox, Loading, PageHeader, useAction } from '../../shared/ui.jsx'
import { bytes, fileMode, isDirMode } from '../format.js'
import { navigate, useHashRoute } from '../hooks.jsx'

const PREVIEW_LIMIT = 1 << 20

function pathFromRoute(route) {
  const q = new URLSearchParams(route.split('?')[1] || '')
  return q.get('path') || '/etc'
}

function go(path) {
  navigate(`/tools/files?${new URLSearchParams({ path })}`)
}

function Crumbs({ path }) {
  const parts = path.split('/').filter(Boolean)
  return (
    <div className="crumbs">
      <button className="ghost small" onClick={() => go('/')}>
        /
      </button>
      {parts.map((p, i) => {
        const to = '/' + parts.slice(0, i + 1).join('/')
        return (
          <span key={to} className="row" style={{ gap: '0.15rem' }}>
            <button className="ghost small" onClick={() => go(to)}>
              {p}
            </button>
            {i < parts.length - 1 && <span className="muted">/</span>}
          </span>
        )
      })}
    </div>
  )
}

function Preview({ path, size }) {
  const [text, setText] = useState(null)
  const [error, setError] = useState(null)
  useEffect(() => {
    setText(null)
    setError(null)
    getText(`/api/files/read?${new URLSearchParams({ path })}`)
      .then(setText)
      .catch(setError)
  }, [path])
  if (error) return <ErrorBox error={error} />
  if (text == null) return <Loading />
  if (text.includes('\u0000')) return <div className="muted">Binary file - download it instead.</div>
  return (
    <>
      {size > PREVIEW_LIMIT && <div className="notice warn">Showing the first 1 MiB of {bytes(size)} - download for the whole file.</div>}
      <pre className="code" style={{ maxHeight: '60vh' }}>
        {text || <span className="muted">(empty file)</span>}
      </pre>
    </>
  )
}

export default function Files() {
  const route = useHashRoute()
  const path = pathFromRoute(route)
  const [entries, setEntries] = useState(null)
  const [error, setError] = useState(null)
  const [selected, setSelected] = useState(null)
  const [jump, setJump] = useState(path)
  const [busy, run] = useAction()

  useEffect(() => {
    setJump(path)
    setEntries(null)
    setError(null)
    setSelected(null)
    getJSON(`/api/files/list?${new URLSearchParams({ path })}`)
      .then((list) => {
        // A file path: show its parent directory with the file selected.
        if (list.length === 1 && list[0].name === path && !isDirMode(list[0].mode)) {
          setEntries(list)
          setSelected(list[0])
          return
        }
        setEntries(list)
      })
      .catch(setError)
  }, [path])

  const sorted = [...(entries || [])].sort((a, b) => Number(isDirMode(b.mode)) - Number(isDirMode(a.mode)) || a.name.localeCompare(b.name))
  const parent = path === '/' ? null : path.replace(/\/[^/]+\/?$/, '') || '/'
  const open = (e) => {
    if (isDirMode(e.mode)) go(e.name)
    else setSelected(e)
  }

  return (
    <>
      <PageHeader
        title="Files"
        subtitle="Read-only access to the node's filesystem"
        actions={
          <form
            className="row"
            onSubmit={(e) => {
              e.preventDefault()
              go(jump)
            }}
          >
            <input className="mono" style={{ width: '20rem' }} value={jump} onChange={(e) => setJump(e.target.value)} />
            <button>Go</button>
          </form>
        }
      />
      <div className="grid grid-2">
        <Card
          title={<Crumbs path={path} />}
          icon={FolderOpen}
          actions={
            <button className="small" disabled={busy} onClick={() => run(() => download(`/api/files/copy?${new URLSearchParams({ path })}`, 'folder.tar'), (r) => `Saved ${r.name}`)}>
              <FileArchive size={14} /> Download as .tar
            </button>
          }
        >
          <ErrorBox error={error} />
          {!entries && !error ? (
            <Loading />
          ) : (
            <div className="table-wrap" style={{ maxHeight: '65vh' }}>
              <table>
                <thead>
                  <tr>
                    <th>Name</th>
                    <th>Mode</th>
                    <th className="num">Size</th>
                  </tr>
                </thead>
                <tbody>
                  {parent && (
                    <tr className="file-row" onClick={() => go(parent)}>
                      <td colSpan={3}>
                        <span className="row">
                          <ArrowUp size={14} /> ..
                        </span>
                      </td>
                    </tr>
                  )}
                  {sorted.map((e) => {
                    const dir = isDirMode(e.mode)
                    const link = fileMode(e.mode).startsWith('l')
                    const Icon = dir ? Folder : link ? Link2 : File
                    return (
                      <tr key={e.name} className="file-row" onClick={() => open(e)} style={selected?.name === e.name ? { background: 'var(--accent-soft)' } : undefined}>
                        <td>
                          <span className="row" style={{ flexWrap: 'nowrap' }}>
                            <Icon size={14} color={dir ? 'var(--accent)' : 'var(--muted)'} />
                            <span className="mono">{e.relative_name || e.name}</span>
                            {e.error && <span className="muted small">({e.error})</span>}
                          </span>
                        </td>
                        <td className="mono small muted">{fileMode(e.mode)}</td>
                        <td className="num small">{dir ? '' : bytes(Number(e.size) || 0)}</td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            </div>
          )}
        </Card>
        <Card
          title={selected ? <span className="mono">{selected.name}</span> : 'Preview'}
          icon={File}
          actions={
            selected && (
              <button className="small" disabled={busy} onClick={() => run(() => download(`/api/files/read?${new URLSearchParams({ path: selected.name, download: 'true' })}`, 'file'), (r) => `Saved ${r.name}`)}>
                <Download size={14} /> Download
              </button>
            )
          }
        >
          {selected ? <Preview path={selected.name} size={Number(selected.size) || 0} /> : <div className="muted">Select a file to preview it.</div>}
        </Card>
      </div>
    </>
  )
}
