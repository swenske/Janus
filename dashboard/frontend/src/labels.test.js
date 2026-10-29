import assert from 'node:assert/strict'
import { test } from 'node:test'
import { effectiveLabels, inheritedRows } from './labels.js'

test("a node's effective labels: its hypervisor's, its own on top", () => {
  const node = { labels: { room: 'b', team: 'web' }, inherited_labels: { dc: 'lgs', room: 'a' }, inherited_from: { kind: 'hypervisor', id: 'h1', name: 'kvm01' } }
  assert.deepEqual(effectiveLabels(node), { dc: 'lgs', room: 'b', team: 'web' })
  assert.deepEqual(inheritedRows(node), [
    { key: 'dc', value: 'lgs', overridden: false },
    { key: 'room', value: 'a', overridden: true },
  ])
  // The same value of its own overrides nothing to show.
  assert.deepEqual(inheritedRows({ labels: { dc: 'lgs' }, inherited_labels: { dc: 'lgs' } }), [{ key: 'dc', value: 'lgs', overridden: false }])
})

test('a node from before inherited labels, or none', () => {
  assert.deepEqual(effectiveLabels({ labels: { team: 'db' } }), { team: 'db' })
  assert.deepEqual(inheritedRows({ labels: { team: 'db' } }), [])
  assert.deepEqual(effectiveLabels(undefined), {})
  assert.deepEqual(inheritedRows({}), [])
})
