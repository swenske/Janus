import { useRef } from 'react'

// Editor is a plain-text editor with line numbers (config files, rulesets).
export function Editor({ value, onChange }) {
  const gutter = useRef(null)
  const lines = value.split('\n').length
  return (
    <div className="editor">
      <div className="editor-gutter" ref={gutter}>
        {Array.from({ length: lines }, (_, i) => (
          <div key={i}>{i + 1}</div>
        ))}
      </div>
      <textarea
        value={value}
        spellCheck={false}
        rows={Math.min(40, Math.max(18, lines + 1))}
        onChange={(e) => onChange(e.target.value)}
        onScroll={(e) => {
          if (gutter.current) gutter.current.scrollTop = e.target.scrollTop
        }}
        onKeyDown={(e) => {
          if (e.key === 'Tab') {
            e.preventDefault()
            const t = e.target
            const { selectionStart: s, selectionEnd: end } = t
            onChange(value.slice(0, s) + '    ' + value.slice(end))
            requestAnimationFrame(() => {
              t.selectionStart = t.selectionEnd = s + 4
            })
          }
        }}
      />
    </div>
  )
}

// DiffView shows a line diff (diff.js's hunks).
export function DiffView({ diff }) {
  return (
    <div className="diff">
      {diff.map((d, i) => (
        <div key={i} className={d.op === '+' ? 'add' : d.op === '-' ? 'del' : 'ctx'}>
          {d.op === '…' ? '  …' : `${d.op} ${d.text}`}
        </div>
      ))}
    </div>
  )
}
