// node --test (npm test)
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { socketKey, socketRates } from './sockets.js'

const conn = (local, remote, rx, tx, protocol = 'tcp') => ({ protocol, local_address: local, remote_address: remote, state: 'ESTABLISHED', rx_bytes: rx, tx_bytes: tx })

test('a connection seen twice has its rates; a new one, a reused one, UDP none', () => {
  const prev = {
    connections: [conn('10.0.2.15:9505', '10.0.2.2:50000', 1000, 5000), conn('10.0.2.15:443', '192.0.2.7:41234', 9000, 9000), { protocol: 'udp', local_address: '0.0.0.0:123', remote_address: '0.0.0.0:0', state: 'UNCONN' }],
  }
  const cur = {
    connections: [
      conn('10.0.2.15:9505', '10.0.2.2:50000', 3000, 9000),
      conn('10.0.2.15:443', '192.0.2.7:41234', 100, 200), // went back: a new connection on the same ports
      conn('10.0.2.15:443', '192.0.2.8:41000', 50, 50), // new
      { protocol: 'udp', local_address: '0.0.0.0:123', remote_address: '0.0.0.0:0', state: 'UNCONN' },
    ],
  }
  const r = socketRates(prev, cur, 2)
  assert.deepEqual(r.get(socketKey(cur.connections[0])), { rx: 1000, tx: 2000 })
  assert.equal(r.size, 1)
  // The first answer: nothing to compare with.
  assert.equal(socketRates(null, cur, 2).size, 0)
  assert.equal(socketRates(prev, cur, 0).size, 0)
})
