// node --test (npm test)
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { backends, ciphers, frontendTraffic, hasIPv6, ipVersions, refusals, responses, tls, tlsVersions, traffic, versions } from './breakdown.js'

const columns = ['pxname', 'svname', 'status', 'mode', 'bin', 'bout', 'hrsp_1xx', 'hrsp_2xx', 'hrsp_3xx', 'hrsp_4xx', 'hrsp_5xx', 'hrsp_other', 'h1req', 'h2req', 'h3req', 'req_other', 'ssl_sess', 'ssl_reused_sess', 'ssl_failed_handshake', 'rtime', 'ctime', 'qtime', 'ttime', 'req_rate', 'rate', 'econ', 'eresp', 'ereq', 'dreq', 'dresp', 'dcon', 'dses', 'wretr', 'wredis']
const row = (o) => columns.map((c) => (o[c] == null ? '' : String(o[c])))
const stats = {
  columns,
  rows: [
    row({ pxname: 'web', svname: 'FRONTEND', status: 'OPEN', mode: 'http', bin: 300, bout: 2000, hrsp_2xx: 90, hrsp_4xx: 7, hrsp_5xx: 3, h1req: 60, h2req: 40, ssl_sess: 30, ssl_reused_sess: 20, ssl_failed_handshake: 2, ereq: 4, dreq: 6, dcon: 1, dses: 1 }),
    row({ pxname: 'tls', svname: 'FRONTEND', status: 'OPEN', mode: 'http', bin: 50, bout: 60, hrsp_2xx: 10, h2req: 10, ssl_sess: 10, ssl_reused_sess: 0 }),
    row({ pxname: 'pg', svname: 'FRONTEND', status: 'OPEN', mode: 'tcp' }),
    row({ pxname: 'app', svname: 'app1', status: 'UP', mode: 'http' }),
    row({ pxname: 'app', svname: 'app2', status: 'DOWN', mode: 'http' }),
    row({ pxname: 'app', svname: 'BACKEND', status: 'UP', mode: 'http', bin: 100, bout: 900, rtime: 12, ttime: 20, req_rate: 5, econ: 1, eresp: 2, dreq: 6, wretr: 3 }),
    row({ pxname: 'api', svname: 'BACKEND', status: 'UP', mode: 'http', bin: 10, bout: 40, rtime: 80, ttime: 90 }),
    row({ pxname: 'db', svname: 'BACKEND', status: 'UP', mode: 'tcp', bin: 5000, bout: 5000, rate: 3 }),
  ],
}

test('responses by class, only those seen', () => {
  assert.deepEqual(
    responses(stats).map((s) => [s.label, s.value]),
    [
      ['2xx', 100],
      ['4xx', 7],
      ['5xx', 3],
    ],
  )
})

test('HTTP versions and TLS handshakes, frontends only', () => {
  assert.deepEqual(
    versions(stats).map((s) => [s.label, s.value]),
    [
      ['HTTP/1.x', 60],
      ['HTTP/2', 50],
    ],
  )
  assert.deepEqual(
    tls(stats).map((s) => [s.label, s.value]),
    [
      ['Full', 40],
      ['Resumed', 20],
      ['Failed', 2],
    ],
  )
})

test('traffic by backend, biggest first, the rest summed', () => {
  assert.deepEqual(
    traffic(stats, 2).map((s) => [s.label, s.value]),
    [
      ['db', 10000],
      ['app', 1000],
      ['1 more', 50],
    ],
  )
})

test('backends: slowest first, servers up, errors', () => {
  const b = backends(stats)
  assert.deepEqual(
    b.map((x) => x.name),
    ['api', 'app', 'db'],
  )
  const app = b.find((x) => x.name === 'app')
  assert.deepEqual([app.servers, app.errors, app.rate], [{ up: 1, all: 2 }, 3, 5])
  assert.equal(b.find((x) => x.name === 'db').rate, 3)
})

test('traffic by frontend, refusals counted once', () => {
  assert.deepEqual(
    frontendTraffic(stats).map((s) => [s.label, s.value]),
    [
      ['web', 2300],
      ['tls', 110],
    ],
  )
  // The backend's dreq is the frontend's denial again: not added.
  assert.deepEqual(
    refusals(stats).map((s) => [s.label, s.value]),
    [
      ['Bad requests', 4],
      ['Denied requests', 6],
      ['Denied connections', 2],
      ['Connection errors', 1],
      ['Response errors', 2],
      ['Retries', 3],
    ],
  )
})

test('nothing to break down', () => {
  for (const f of [responses, versions, tls, traffic, frontendTraffic, refusals, backends]) assert.deepEqual(f(null), [])
  assert.deepEqual(ipVersions(null), [])
})

test('IP versions of the logged clients; IPv6 of the node beyond its links', () => {
  assert.deepEqual(
    ipVersions({ families: [{ name: 'IPv4', count: 80 }, { name: 'IPv6', count: 20 }] }).map((s) => [s.label, s.value]),
    [
      ['IPv4', 80],
      ['IPv6', 20],
    ],
  )
  const iface = (kind, ...addresses) => ({ kind, addresses })
  assert.equal(hasIPv6({ interfaces: [iface('loopback', '127.0.0.1/8', '::1/128'), iface('physical', '10.0.2.15/24', 'fe80::5054:ff:fe12:3456/64')] }), false)
  assert.equal(hasIPv6({ interfaces: [iface('physical', '10.0.2.15/24'), iface('vlan', '2001:db8::10/64')] }), true)
  assert.equal(hasIPv6(null), false)
})

test('TLS versions coloured by age, ciphers the biggest five and the rest', () => {
  const log = {
    lines: 100,
    tls: 90,
    versions: [
      { name: 'TLSv1.3', count: 70 },
      { name: 'TLSv1.2', count: 19 },
      { name: 'TLSv1', count: 1 },
    ],
    ciphers: ['A', 'B', 'C', 'D', 'E', 'F', 'G'].map((n, i) => ({ name: `CIPHER_${n}`, count: 40 - i * 5 })),
  }
  assert.deepEqual(
    tlsVersions(log).map((s) => [s.label, s.value, s.color]),
    [
      ['TLSv1.3', 70, 'var(--ok)'],
      ['TLSv1.2', 19, 'var(--info)'],
      ['TLSv1', 1, 'var(--danger)'],
    ],
  )
  const c = ciphers(log)
  assert.deepEqual(
    c.map((s) => [s.label, s.value]),
    [
      ['CIPHER_A', 40],
      ['CIPHER_B', 35],
      ['CIPHER_C', 30],
      ['CIPHER_D', 25],
      ['CIPHER_E', 20],
      ['2 more', 25],
    ],
  )
  assert.deepEqual(tlsVersions(null), [])
  assert.deepEqual(ciphers({ ciphers: [] }), [])
})
