// node --test (npm test)
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { DEFAULT_PREFERENCES, withChoice } from './preferences.js'

test("a change sends the account's own choices with it, not the defaults", () => {
  assert.deepEqual(withChoice({}, 'node_stats', false), { node_stats: false })
  assert.deepEqual(withChoice({ nodes_refresh_seconds: 30 }, 'node_charts', false), { nodes_refresh_seconds: 30, node_charts: false })
  assert.deepEqual(withChoice(undefined, 'hypervisors_refresh_seconds', 60), { hypervisors_refresh_seconds: 60 })
})

test('the defaults are the pages as they were', () => {
  assert.equal(DEFAULT_PREFERENCES.nodes_refresh_seconds, 15)
  assert.equal(DEFAULT_PREFERENCES.node_stats, true)
  assert.equal(DEFAULT_PREFERENCES.node_charts, true)
  assert.ok(DEFAULT_PREFERENCES.choices.nodes.includes(DEFAULT_PREFERENCES.nodes_refresh_seconds))
  assert.ok(DEFAULT_PREFERENCES.choices.hypervisors.includes(DEFAULT_PREFERENCES.hypervisors_refresh_seconds))
})
