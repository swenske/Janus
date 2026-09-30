import { ArrowDown, ArrowUp, Search } from 'lucide-react'
import { useMemo, useState } from 'react'

// DataTable: sortable columns, optional text filter across every column.
// columns: [{key, label, get?(row), render?(row), num?, width?}]
export default function DataTable({ columns, rows, filterable = true, initialSort, empty = 'Nothing to show', maxHeight, toolbar }) {
  const [sort, setSort] = useState(initialSort || { key: null, dir: 1 })
  const [q, setQ] = useState('')

  const shown = useMemo(() => {
    const value = (col, row) => (col.get ? col.get(row) : row[col.key])
    let out = rows || []
    if (q) {
      const needle = q.toLowerCase()
      out = out.filter((r) => columns.some((c) => String(value(c, r) ?? '').toLowerCase().includes(needle)))
    }
    if (sort.key) {
      const col = columns.find((c) => c.key === sort.key)
      out = [...out].sort((a, b) => {
        const va = value(col, a)
        const vb = value(col, b)
        if (va == null) return 1
        if (vb == null) return -1
        return (typeof va === 'number' && typeof vb === 'number' ? va - vb : String(va).localeCompare(String(vb), undefined, { numeric: true })) * sort.dir
      })
    }
    return out
  }, [rows, columns, q, sort])

  return (
    <div className="stack">
      {(filterable || toolbar) && (
        <div className="row">
          {filterable && (
            <label className="search">
              <Search size={14} />
              <input placeholder="Filter…" value={q} onChange={(e) => setQ(e.target.value)} />
            </label>
          )}
          <span className="muted small">
            {shown.length}
            {rows && shown.length !== rows.length ? ` of ${rows.length}` : ''} rows
          </span>
          <span className="grow" />
          {toolbar}
        </div>
      )}
      <div className="table-wrap" style={maxHeight ? { maxHeight } : undefined}>
        <table>
          <thead>
            <tr>
              {columns.map((c) => (
                <th
                  key={c.key}
                  className={`sortable ${c.num ? 'num' : ''}`}
                  style={c.width ? { width: c.width } : undefined}
                  onClick={() => setSort((s) => ({ key: c.key, dir: s.key === c.key ? -s.dir : c.num ? -1 : 1 }))}
                >
                  <span className="row" style={{ gap: '0.2rem', justifyContent: c.num ? 'flex-end' : 'flex-start', flexWrap: 'nowrap' }}>
                    {c.label}
                    {sort.key === c.key && (sort.dir > 0 ? <ArrowUp size={12} /> : <ArrowDown size={12} />)}
                  </span>
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {shown.length === 0 ? (
              <tr>
                <td colSpan={columns.length} className="empty">
                  {empty}
                </td>
              </tr>
            ) : (
              shown.map((r, i) => (
                <tr key={r.__key ?? i}>
                  {columns.map((c) => (
                    <td key={c.key} className={c.num ? 'num' : ''}>
                      {c.render ? c.render(r) : (c.get ? c.get(r) : r[c.key]) ?? ''}
                    </td>
                  ))}
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}
