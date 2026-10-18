import { WINDOWS } from './chartWindow.js'

// WindowPicker chooses the time a node page's charts show.
export default function WindowPicker({ value, onChange }) {
  return (
    <div className="row" role="group" aria-label="Time shown">
      {WINDOWS.map((w) => (
        <button key={w.label} className={`small ${value === w.ms ? 'active' : ''}`} aria-pressed={value === w.ms} onClick={() => onChange(w.ms)}>
          {w.label}
        </button>
      ))}
    </div>
  )
}
