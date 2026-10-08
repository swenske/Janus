import { AlertTriangle, Loader2, X } from 'lucide-react'
import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'

export function Card({ title, icon: Icon, actions, children, className = '' }) {
  return (
    <section className={`card ${className}`}>
      {(title || actions) && (
        <div className="card-header">
          <div className="card-title">
            {Icon && <Icon size={16} />}
            {title}
          </div>
          {actions && <div className="row">{actions}</div>}
        </div>
      )}
      {children}
    </section>
  )
}

export function Stat({ label, value, sub, tone }) {
  return (
    <div className="card stat">
      <div className="stat-label">{label}</div>
      <div className="stat-value" style={tone ? { color: `var(--${tone})` } : undefined}>
        {value}
      </div>
      {sub && <div className="stat-sub">{sub}</div>}
    </div>
  )
}

export function Badge({ tone, children, dot }) {
  return (
    <span className={`badge ${tone || ''}`}>
      {dot && <span className="dot" />}
      {children}
    </span>
  )
}

export function stateTone(state) {
  const s = String(state || '').toLowerCase()
  if (['running', 'healthy', 'up', 'open', 'live', 'ready', 'listen', 'established', 'no check'].some((k) => s.startsWith(k))) return 'ok'
  if (['drain', 'maint', 'unknown', 'reconnecting', 'connecting', 'not_enabled', 'paused', 'nolb'].some((k) => s.startsWith(k))) return 'warn'
  if (['down', 'failed', 'stopped', 'error', 'unhealthy', 'unreachable'].some((k) => s.startsWith(k))) return 'danger'
  return ''
}

export function Loading({ label = 'Loading…' }) {
  return (
    <div className="empty row" style={{ justifyContent: 'center' }}>
      <Loader2 size={16} className="spin" /> {label}
    </div>
  )
}

export function ErrorBox({ error }) {
  if (!error) return null
  return <div className="error-box">{String(error.message || error)}</div>
}

export function Empty({ children }) {
  return <div className="empty">{children}</div>
}

export function Tabs({ tabs, active, onChange }) {
  return (
    <div className="tabs" role="tablist">
      {tabs.map((t) => (
        <button key={t.id} role="tab" aria-selected={active === t.id} className={active === t.id ? 'tab active' : 'tab'} onClick={() => onChange(t.id)}>
          {t.icon && <t.icon size={15} />}
          {t.label}
        </button>
      ))}
    </div>
  )
}

export function PageHeader({ title, subtitle, actions }) {
  return (
    <div className="page-header">
      <div>
        <h1>{title}</h1>
        {subtitle && <div className="muted">{subtitle}</div>}
      </div>
      {actions && <div className="row">{actions}</div>}
    </div>
  )
}

export function Meter({ value, max, tone }) {
  const pct = max > 0 ? Math.min(100, (value / max) * 100) : 0
  const t = tone || (pct > 90 ? 'danger' : pct > 75 ? 'warn' : 'ok')
  return (
    <div className={`bar ${t}`} title={`${pct.toFixed(1)}%`}>
      <div style={{ width: `${pct}%` }} />
    </div>
  )
}

// --- toasts ---

const ToastContext = createContext(() => {})

export function ToastProvider({ children }) {
  const [toasts, setToasts] = useState([])
  const push = useCallback((message, tone = 'ok') => {
    const id = Math.random().toString(36).slice(2)
    setToasts((t) => [...t, { id, message: String(message?.message || message), tone }])
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), tone === 'danger' ? 9000 : 4500)
  }, [])
  return (
    <ToastContext.Provider value={push}>
      {children}
      <div className="toasts" aria-live="polite">
        {toasts.map((t) => (
          <div key={t.id} className={`toast ${t.tone}`}>
            <span className="grow">{t.message}</span>
            <button className="ghost icon" aria-label="Dismiss" onClick={() => setToasts((all) => all.filter((x) => x.id !== t.id))}>
              <X size={14} />
            </button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  )
}

export function useToast() {
  return useContext(ToastContext)
}

// --- confirmation dialog ---

const ConfirmContext = createContext(async () => false)

// useConfirm returns confirm({title, body, action, danger, typeToConfirm})
// -> Promise<boolean>. typeToConfirm makes the user type a word first,
// for actions that can't be undone.
export function ConfirmProvider({ children }) {
  const [dialog, setDialog] = useState(null)
  const [typed, setTyped] = useState('')
  const confirm = useCallback(
    (opts) =>
      new Promise((resolve) => {
        setTyped('')
        setDialog({ ...opts, resolve })
      }),
    [],
  )
  const close = (result) => {
    dialog?.resolve(result)
    setDialog(null)
  }
  useEffect(() => {
    if (!dialog) return undefined
    const onKey = (e) => e.key === 'Escape' && close(false)
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  })
  const blocked = dialog?.typeToConfirm && typed !== dialog.typeToConfirm
  return (
    <ConfirmContext.Provider value={confirm}>
      {children}
      {dialog && (
        <div className="modal-backdrop confirm" onClick={() => close(false)}>
          <div className={`modal card ${dialog.wide ? 'wide' : ''}`} role="dialog" aria-modal="true" onClick={(e) => e.stopPropagation()}>
            <div className="row" style={{ marginBottom: '0.6rem' }}>
              {dialog.danger && <AlertTriangle size={18} color="var(--danger)" />}
              <h2>{dialog.title}</h2>
            </div>
            <div className="stack">{dialog.body}</div>
            {dialog.typeToConfirm && (
              <label className="field" style={{ marginTop: '0.8rem' }}>
                <span>
                  Type <strong className="mono">{dialog.typeToConfirm}</strong> to confirm
                </span>
                <input autoFocus value={typed} onChange={(e) => setTyped(e.target.value)} />
              </label>
            )}
            <div className="row" style={{ justifyContent: 'flex-end', marginTop: '1rem' }}>
              <button onClick={() => close(false)}>Cancel</button>
              <button className={dialog.danger ? 'danger solid' : 'primary'} disabled={blocked} onClick={() => close(true)}>
                {dialog.action || 'Confirm'}
              </button>
            </div>
          </div>
        </div>
      )}
    </ConfirmContext.Provider>
  )
}

export function useConfirm() {
  return useContext(ConfirmContext)
}

// useAction wraps an async operation with busy state and toasts.
export function useAction() {
  const toast = useToast()
  const [busy, setBusy] = useState(false)
  const run = useCallback(
    async (fn, success) => {
      setBusy(true)
      try {
        const result = await fn()
        if (success) toast(typeof success === 'function' ? success(result) : success)
        return result
      } catch (err) {
        toast(err, 'danger')
        return undefined
      } finally {
        setBusy(false)
      }
    },
    [toast],
  )
  return useMemo(() => [busy, run], [busy, run])
}
