// BrandMark is Janus's symbol drawn in the page's palette - the bar in its
// accent, the faces in its text colour (the splash's drawing): the
// favicon's own colours follow only the system's mode. Decorative: it's
// always beside the name. An icon's props too (size, className, x and y
// inside an SVG): the Traffic flow draws it as its middle node's.
export default function BrandMark({ size = 28, className = '', x, y }) {
  return (
    <svg className={`brand-mark ${className}`} viewBox="8 8 84 84" width={size} height={size} x={x} y={y} aria-hidden="true" focusable="false">
      <rect className="brand-bar" x="42" y="12" width="16" height="76" rx="3" />
      <path className="brand-face" d="M12 50L37 30V70Z" />
      <path className="brand-face" d="M88 50L63 30V70Z" />
    </svg>
  )
}
