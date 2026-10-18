import Chart from './Chart.jsx'

// Sparkline is a chart reduced to its curve: no axes, no legend, its
// first series filled - a card's live trend.
export default function Sparkline({ height = 34, ...props }) {
  return <Chart compact height={height} {...props} />
}
