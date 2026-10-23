import { LayoutGrid, List } from 'lucide-react'

// ViewToggle: the two buttons choosing how a page shows its things -
// cards, or rows of a table (storedView.js remembers which).
export default function ViewToggle({ value, onChange, label }) {
  return (
    <div className="segmented" role="group" aria-label={label}>
      <button className={`small ${value === 'cards' ? 'active' : ''}`} aria-pressed={value === 'cards'} onClick={() => onChange('cards')}>
        <LayoutGrid size={14} /> Cards
      </button>
      <button className={`small ${value === 'list' ? 'active' : ''}`} aria-pressed={value === 'list'} onClick={() => onChange('list')}>
        <List size={14} /> List
      </button>
    </div>
  )
}
