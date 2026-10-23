// Where an exporter listens (listen.test.js) - its settings' address:
// "auto", the node's main interface (the one its IPv4 default route
// leaves by: the default), "" every address, else one IP of the node.
export const AUTO = 'auto'

// listenMode is the choice an address is: auto, all, one.
export function listenMode(address) {
  return address === AUTO ? 'auto' : address ? 'one' : 'all'
}

// listenAddress is the address a choice saves.
export function listenAddress(mode, ip) {
  return mode === 'auto' ? AUTO : mode === 'all' ? '' : ip.trim()
}

// listenSaid: the address in words.
export function listenSaid(address) {
  return address === AUTO ? 'the main interface' : address || 'every address'
}

// scrapeHost is where Prometheus reaches it: an address it listens on
// (auto's, resolved by the node), else the one set, else the node's own
// (/api/node's, its port dropped) - bracketed for a URL.
export function scrapeHost(address, effective, nodeAddress) {
  const host = effective?.[0] || (address && address !== AUTO ? address : (nodeAddress || '').replace(/:\d+$/, '').replace(/^\[|\]$/g, '')) || '<node>'
  return host.includes(':') ? `[${host}]` : host
}
