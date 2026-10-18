// BrandMark is Janus's symbol drawn in the page's palette - the bar in its
// accent, the faces in its text colour (the splash's drawing): the
// favicon's own colours follow only the system's mode. Decorative: it's
// always beside the name.
export default function BrandMark({ size = 28 }) {
  return (
    <svg className="brand-mark" viewBox="8 8 84 84" width={size} height={size} aria-hidden="true" focusable="false">
      <rect className="brand-bar" x="42" y="12" width="16" height="76" rx="3" />
      <path className="brand-face" d="M12 50L37 30V70Z" />
      <path className="brand-face" d="M88 50L63 30V70Z" />
    </svg>
  )
}
