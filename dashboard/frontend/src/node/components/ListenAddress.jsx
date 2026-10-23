// ListenAddress: where an exporter listens (listen.js) - the node's main
// interface, every address, or one address typed in.
export default function ListenAddress({ label, mode, ip, onMode, onIP, disabled }) {
  return (
    <>
      <label className="field" style={{ width: '13rem' }}>
        <span>{label}</span>
        <select value={mode} onChange={(e) => onMode(e.target.value)} disabled={disabled}>
          <option value="auto">Main interface</option>
          <option value="all">Every address</option>
          <option value="one">One address</option>
        </select>
      </label>
      {mode === 'one' && (
        <label className="field" style={{ width: '13rem' }}>
          <span>Address</span>
          <input className="mono" value={ip} placeholder="one of the node's IPs" onChange={(e) => onIP(e.target.value.trim())} disabled={disabled} />
        </label>
      )}
    </>
  )
}
