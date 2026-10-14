import { useEffect, useState } from 'react'
import { call } from './call.js'

// A machine's auto-scaling, the data side (Autoscale.jsx draws it).

// The thresholds a form shows, each with the policy's field, its unit
// (seconds shown in minutes) and its words.
export const THRESHOLDS = [
  { key: 'cpu_up_idle_percent', label: 'A vCPU more when HAProxy is idle less than', unit: '%' },
  { key: 'cpu_up_busy_percent', label: '… or the CPUs are busier than', unit: '%' },
  { key: 'cpu_up_after_seconds', label: '… on average for', unit: 'min' },
  { key: 'cpu_down_idle_percent', label: 'A vCPU less when HAProxy would stay idle more than', unit: '%' },
  { key: 'memory_up_available_percent', label: 'More memory when less is available than', unit: '%' },
  { key: 'memory_up_after_seconds', label: '… for', unit: 'min' },
  { key: 'memory_down_available_percent', label: 'Less memory when more would stay available than', unit: '%' },
  { key: 'return_after_seconds', label: 'A step back towards its size after a quiet', unit: 'min' },
  { key: 'cooldown_seconds', label: 'At least between two steps up', unit: 'min' },
  { key: 'max_steps_per_hour', label: 'Steps an hour, at most', unit: '' },
]

const shown = (t, v) => (t.unit === 'min' ? Math.round((v / 60) * 10) / 10 : v)
const stored = (t, v) => (t.unit === 'min' ? Math.round(Number(v) * 60) : Number(v))

// useAutoscale reads a machine's auto-scaling: policy, state, journal and
// the defaults.
export function useAutoscale(id, deps = []) {
  const [data, setData] = useState(null)
  const [error, setError] = useState(null)
  useEffect(() => {
    let live = true
    call(`/api/machines/${id}/autoscale`)
      .then((d) => live && setData(d))
      .catch((e) => live && setError(e))
    return () => {
      live = false
    }
  }, [id, ...deps]) // eslint-disable-line react-hooks/exhaustive-deps
  return [data, error]
}

// initialAutoscale is the form's state for a machine: "none" without a
// policy, its mode, maxima and thresholds - the defaults where it has
// none.
export function initialAutoscale(m, defaults) {
  const p = m.spec.autoscale
  const values = {}
  for (const t of THRESHOLDS) {
    const v = p?.[t.key] || defaults?.[t.key] || 0
    values[t.key] = shown(t, v)
  }
  return {
    mode: p ? p.mode : 'none',
    max_vcpus: p ? p.max_vcpus : Math.min(64, m.spec.vcpus * 2),
    max_memory_mib: p ? p.max_memory_mib : m.spec.memory_mib * 4,
    values,
  }
}

// autoscalePolicy is the form's policy for the API: null without one; a
// threshold at its default is left out, so it follows the default.
export function autoscalePolicy(form, defaults) {
  if (form.mode === 'none') return null
  const out = { mode: form.mode, max_vcpus: Number(form.max_vcpus), max_memory_mib: Number(form.max_memory_mib) }
  for (const t of THRESHOLDS) {
    const v = stored(t, form.values[t.key])
    if (v && v !== defaults?.[t.key]) out[t.key] = v
  }
  return out
}

// samePolicy compares two policies as the API keeps them.
export function samePolicy(a, b) {
  return JSON.stringify(a || null) === JSON.stringify(b || null)
}

// sizeText is a machine's size on its card: its spec, and what it has
// now - or, observing, what it would have - when that differs.
export function sizeText(m) {
  const base = `${m.spec.vcpus} vCPU · ${m.spec.memory_mib} MiB`
  const p = m.spec.autoscale
  const st = m.autoscale_state
  if (p?.mode === 'observe' && st?.size && (st.size.vcpus !== m.spec.vcpus || st.size.memory_mib !== m.spec.memory_mib)) {
    return `${base} - would be ${st.size.vcpus} vCPU · ${st.size.memory_mib} MiB`
  }
  const now = m.size
  if (now && (now.vcpus !== m.spec.vcpus || now.memory_mib !== m.spec.memory_mib)) {
    return `${base} - now ${now.vcpus} vCPU · ${now.memory_mib} MiB`
  }
  return base
}
