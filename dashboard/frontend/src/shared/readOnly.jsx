import { Eye, Pencil } from 'lucide-react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { lastEventAt } from './activity.js'
import { current, EditingContext, store, useEditing } from './editing.js'
import { held, lastReleaseAt } from './holds.js'
import { useToast } from './ui.jsx'

// The Read-only switch (#527, docs/controller-ui.md "Read-only mode"):
// while it's on, the pages show nothing that changes a node, a cluster or
// the Controller - what reads and diagnoses stays. A convenience of the
// pages, never a permission: the Controller and the nodes decide anyway.
// Its state is the tab's (shared/editing.js). In edit mode the page goes
// back to read-only once nobody touched it for the account's delay
// (read_only_after_minutes) - counted across the tab's pages, from the
// wall clock (a sleeping laptop's timers stop), and never while an action
// is under way (shared/holds.js).

const TICK = 15 * 1000

// ReadOnlyProvider keeps the switch for the page below it. prefs are the
// account's (pages_open, read_only_after_minutes), loaded once they came.
export function ReadOnlyProvider({ prefs, loaded, children }) {
  const pagesOpen = prefs?.pages_open === 'read_write' ? 'read_write' : 'read_only'
  // An account whose pages open read-write isn't put back into read-only
  // behind its back.
  const after = pagesOpen === 'read_write' ? 0 : (prefs?.read_only_after_minutes ?? 15)
  // chosen: the tab's own choice (null: none - the preference decides;
  // before it comes, read-only: never a flash of buttons the account
  // didn't want).
  const [chosen, setChosen] = useState(() => current()?.editing ?? null)
  const editing = chosen ?? (loaded ? pagesOpen === 'read_write' : false)
  const toast = useToast()
  const setEditing = useCallback((on) => {
    store({ editing: on, at: Date.now() })
    setChosen(on)
  }, [])

  useEffect(() => {
    if (!editing || !after) return undefined
    const check = () => {
      const s = current() || { editing: true, at: Date.now() }
      const last = Math.max(s.at || 0, lastEventAt(), lastReleaseAt())
      if (last > (s.at || 0)) store({ editing: true, at: last })
      if (held()) return
      if (Date.now() - last >= after * 60 * 1000) {
        store({ editing: false, at: Date.now() })
        setChosen(false)
        toast(`Back to read-only after ${after} min without activity.`, 'warn')
      }
    }
    // A tab reopened after a night: read-only at once.
    const first = setTimeout(check, 0)
    const timer = setInterval(check, TICK)
    const onShow = () => {
      if (!document.hidden) check()
    }
    // Leaving for another page of the tab: the count goes with it.
    const keep = () => {
      const s = current()
      if (s?.editing && lastEventAt() > (s.at || 0)) store({ editing: true, at: lastEventAt() })
    }
    document.addEventListener('visibilitychange', onShow)
    window.addEventListener('focus', onShow)
    window.addEventListener('pagehide', keep)
    return () => {
      clearTimeout(first)
      clearInterval(timer)
      document.removeEventListener('visibilitychange', onShow)
      window.removeEventListener('focus', onShow)
      window.removeEventListener('pagehide', keep)
    }
  }, [editing, after, toast])

  const value = useMemo(() => ({ editing, after, setEditing }), [editing, after, setEditing])
  return <EditingContext.Provider value={value}>{children}</EditingContext.Provider>
}

// ReadOnlySwitch is the header's switch, left of Refresh: nothing for an
// account that may change nothing here (available false).
export function ReadOnlySwitch({ available }) {
  const { editing, after, setEditing } = useEditing()
  if (!available) return null
  const readOnly = !editing
  const title = readOnly
    ? 'Read-only: nothing on the page changes a node, a cluster or the Controller. Click to edit.'
    : `Editing: the page offers every change your role allows${after ? ` - back to read-only after ${after} min without activity` : ''}. Click for read-only.`
  return (
    <button type="button" className={`ghost read-only-switch ${readOnly ? 'read-only' : 'editing'}`} aria-pressed={!readOnly} title={title} onClick={() => setEditing(readOnly)}>
      {readOnly ? <Eye size={15} aria-hidden="true" /> : <Pencil size={15} aria-hidden="true" />}
      <span className="read-only-label">{readOnly ? 'Read-only' : 'Editing'}</span>
    </button>
  )
}
