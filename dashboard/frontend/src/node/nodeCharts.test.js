// node --test (npm test)
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { nodeChartsFrom, readNodeCharts } from './nodeCharts.js'

test('node charts: on unless the account says off', () => {
  assert.equal(nodeChartsFrom({ node_charts: false }), false)
  assert.equal(nodeChartsFrom({ node_charts: true }), true)
  assert.equal(nodeChartsFrom({}), true)
  assert.equal(nodeChartsFrom(null), true)
  assert.equal(nodeChartsFrom({ node_charts: 0 }), true) // only false says off
})

test('node charts: read from the Controller, on when it has no such route', async (t) => {
  const real = globalThis.fetch
  t.after(() => (globalThis.fetch = real))
  const answer = (status, body) => async (url, opts) => {
    assert.equal(url, '/api/me/preferences') // the Controller's, not the node's
    assert.equal(opts.headers['X-Janus-Background'], '1')
    return new Response(body, { status })
  }
  globalThis.fetch = answer(200, '{"node_charts":false}')
  assert.equal(await readNodeCharts(), false)
  globalThis.fetch = answer(200, '{"node_charts":true}')
  assert.equal(await readNodeCharts(), true)
  globalThis.fetch = answer(200, '{}')
  assert.equal(await readNodeCharts(), true)
  globalThis.fetch = answer(404, 'not found') // a Controller older than the setting
  assert.equal(await readNodeCharts(), true)
  globalThis.fetch = answer(200, 'not json')
  assert.equal(await readNodeCharts(), true)
  globalThis.fetch = async () => {
    throw new TypeError('network down')
  }
  assert.equal(await readNodeCharts(), true)
})
