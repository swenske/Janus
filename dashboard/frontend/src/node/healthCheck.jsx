import { Loader2, TriangleAlert, X } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { getJSON } from './api.js'
import { healthCheckOutcome, undoneUpdate } from './bootCommit.js'
import { dateTime } from './format.js'
import { useNodeStatus } from './hooks.jsx'

// HealthCheck follows an update made with "Revert automatically" once the
// node answers on its new slot: janusd checks HAProxy there and either
// confirms the boot or goes back to the previous slot by itself, about
// the health timeout later (#726 - the page said "Back on" at the first
// answer, then nothing when the node went back). events: useNodeEvents'.
// onDone gets { outcome: 'confirmed' | 'reverted' | 'unknown', error,
// now, nowSlot } - now/nowSlot what the node runs after a revert.
export function HealthCheck({ events, from, to, slot, bootUnix, timeoutS, onDone }) {
  const { reload } = useNodeStatus()
  const [elapsed, setElapsed] = useState(0)
  const done = useRef(false)
  const { outcome, revertTo, error } = healthCheckOutcome(events, { slot, sinceNs: bootUnix * 1e9 })
  const finish = useRef(null)
  finish.current = (r) => {
    if (done.current) return
    done.current = true
    reload()
    onDone(r)
  }

  useEffect(() => {
    const started = Date.now()
    const t = setInterval(() => setElapsed(Math.round((Date.now() - started) / 1000)), 1000)
    return () => clearInterval(t)
  }, [])

  useEffect(() => {
    if (outcome === 'confirmed') finish.current({ outcome, error })
  }, [outcome, error])

  // Reverted: the node reboots into the previous slot - done once it
  // answers from another boot.
  useEffect(() => {
    if (outcome !== 'reverted') return undefined
    let stopped = false
    let t = null
    const tick = async () => {
      if (stopped) return
      try {
        const o = await getJSON('/api/system/overview')
        if (o.system?.boot_time_unix && o.system.boot_time_unix !== bootUnix) {
          finish.current({ outcome, error, now: o.version?.version, nowSlot: o.version?.active_slot || revertTo })
          return
        }
      } catch {
        // rebooting
      }
      t = setTimeout(tick, 2000)
    }
    t = setTimeout(tick, 2000)
    return () => {
      stopped = true
      clearTimeout(t)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [outcome])

  // No word by the health timeout, the revert's reboot and some margin:
  // the page stops waiting (the node's Events say what happened).
  const limit = (timeoutS || 60) + 120
  useEffect(() => {
    if (outcome === 'pending' && elapsed > limit) finish.current({ outcome: 'unknown', error })
  }, [elapsed, outcome, limit, error])

  return (
    <div className="notice warn">
      <div className="row">
        <Loader2 size={16} className="spin" />
        <span>
          {outcome === 'reverted' ? (
            <>
              HAProxy didn&apos;t come up healthy on {to}: the node is going back to {from} (slot {revertTo}) by itself
            </>
          ) : (
            <>
              On {to} (slot {slot}) - waiting for HAProxy to answer healthy, up to {timeoutS || 60} s; otherwise the node goes back to {from} by itself
            </>
          )}{' '}
          - {elapsed}s
        </span>
      </div>
      {error && (
        <div className="small mono" style={{ marginTop: '0.4rem', overflowWrap: 'anywhere' }}>
          HAProxy: {error}
        </div>
      )}
    </div>
  )
}

// UndoneUpdate says so when the node's last update didn't hold and the
// node still runs the slot it went back to - from its event log, so a
// page opened later says it too. Hidden for that revert once dismissed.
export function UndoneUpdate({ events, activeSlot }) {
  const u = undoneUpdate(events, activeSlot)
  const key = u ? `janus.update.undone-seen.${u.slot}.${u.timeNs}` : ''
  const [dismissed, setDismissed] = useState('')
  let seen = dismissed === key
  try {
    seen = seen || (key && localStorage.getItem(key) === '1')
  } catch {
    // no storage
  }
  if (!u || seen) return null
  const dismiss = () => {
    setDismissed(key)
    try {
      localStorage.setItem(key, '1')
    } catch {
      // no storage: hidden until the page is reloaded
    }
  }
  return (
    <div className="notice danger" style={{ marginBottom: '1rem' }}>
      <div className="row">
        <TriangleAlert size={16} />
        <span className="grow">
          <strong>The last update didn&apos;t hold.</strong> HAProxy didn&apos;t come up healthy on slot {u.slot}
          {u.version ? ` (${u.version})` : ''}, so the node went back to slot {u.revertTo} by itself at {dateTime(u.timeNs / 1e6)}. Slot {u.slot} keeps that
          release: fix what HAProxy refuses there before updating again.
        </span>
        <button className="small ghost" onClick={dismiss} title="Hide this notice" aria-label="Hide this notice">
          <X size={14} />
        </button>
      </div>
      {u.error && (
        <div className="small mono" style={{ marginTop: '0.4rem', overflowWrap: 'anywhere' }}>
          HAProxy: {u.error}
        </div>
      )}
    </div>
  )
}
