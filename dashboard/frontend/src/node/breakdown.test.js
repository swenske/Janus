// node --test (npm test)
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { backends, responses, tls, traffic, versions } from './breakdown.js'

const columns = ['pxname', 'svname', 'status', 'mode', 'bin', 'bout', 'hrsp_1xx', 'hrsp_2xx', 'hrsp_3xx', 'hrsp_4xx', 'hrsp_5xx', 'hrsp_other', 'h1req', 'h2req', 'h3req', 'req_other', 'ssl_sess', 'ssl_reused_sess', 'ssl_failed_handshake', 'rtime', 'ctime', 'qtime', 'ttime', 'req_rate', 'rate', 'econ', 'eresp']
const row = (o) => columns.map((c) => (o[c] == null ? '' : String(o[c])))
const stats = {
  columns,
  rows: [
    row({ pxname: 'web', svname: 'FRONTEND', status: 'OPEN', mode: 'http', hrsp_2xx: 90, hrsp_4xx: 7, hrsp_5xx: 3, h1req: 60, h2req: 40, ssl_sess: 30, ssl_reused_sess: 20, ssl_failed_handshake: 2 }),
    row({ pxname: 'tls', svname: 'FRONTEND', status: 'OPEN', mode: 'http', hrsp_2xx: 10, h2req: 10, ssl_sess: 10, ssl_reused_sess: 0 }),
    row({ pxname: 'pg', svname: 'FRONTEND', status: 'OPEN', mode: 'tcp' }),
    row({ pxname: 'app', svname: 'app1', status: 'UP', mode: 'http' }),
    row({ pxname: 'app', svname: 'app2', status: 'DOWN', mode: 'http' }),
    row({ pxname: 'app', svname: 'BACKEND', status: 'UP', mode: 'http', bin: 100, bout: 900, rtime: 12, ttime: 20, req_rate: 5, econ: 1, eresp: 2 }),
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

test('nothing to break down', () => {
  for (const f of [responses, versions, tls, traffic, backends]) assert.deepEqual(f(null), [])
})
