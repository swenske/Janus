import assert from 'node:assert/strict'
import { test } from 'node:test'
import { dependentsOf, requirementError, requirementsOf, toggleExtension, withExtension, withoutExtension } from './requires.js'

const catalog = [
  { name: 'bird', replaces: ['bird2'] },
  { name: 'prometheus-bird-exporter', requires: ['bird'] },
  { name: 'old-style', requires: ['bird2'] },
  { name: 'orphan', requires: ['gone'] },
  { name: 'nftables' },
]

// The image factory's words (internal/schematic's tests hold the same).
test('requirementError', () => {
  assert.equal(requirementError(['prometheus-bird-exporter', 'bird'], catalog, 'v2026.10.29'), '')
  assert.equal(requirementError(['bird2', 'prometheus-bird-exporter'], catalog, 'v2026.10.29'), '')
  assert.equal(requirementError(['old-style', 'bird'], catalog, 'v2026.10.29'), '')
  assert.equal(requirementError(['prometheus-bird-exporter'], catalog, 'v2026.10.29'), 'extension "prometheus-bird-exporter" requires the "bird" extension: choose it too')
  assert.equal(requirementError(['old-style', 'nftables'], catalog, 'v2026.10.29'), 'extension "old-style" requires the "bird" extension: choose it too')
  assert.equal(requirementError(['orphan'], catalog, 'v2026.10.29'), 'extension "orphan" requires the "gone" extension, which Janus v2026.10.29 doesn\'t offer')
  assert.equal(requirementError([], undefined, ''), '')
})

test('ticking an extension ticks its requirement', () => {
  assert.deepEqual(requirementsOf('prometheus-bird-exporter', catalog), ['bird'])
  assert.deepEqual(requirementsOf('old-style', catalog), ['bird'])
  assert.deepEqual(withExtension(['nftables'], 'prometheus-bird-exporter', catalog), { extensions: ['nftables', 'prometheus-bird-exporter', 'bird'], added: ['bird'] })
  assert.deepEqual(withExtension(['bird'], 'prometheus-bird-exporter', catalog), { extensions: ['bird', 'prometheus-bird-exporter'], added: [] })
  assert.deepEqual(toggleExtension([], 'prometheus-bird-exporter', true, catalog), {
    extensions: ['prometheus-bird-exporter', 'bird'],
    note: 'bird ticked too: prometheus-bird-exporter requires it.',
  })
  assert.deepEqual(toggleExtension([], 'nftables', true, catalog), { extensions: ['nftables'], note: '' })
})

test('unticking a requirement unticks what requires it', () => {
  assert.deepEqual(dependentsOf('bird', catalog).sort(), ['old-style', 'prometheus-bird-exporter'])
  assert.deepEqual(withoutExtension(['bird', 'prometheus-bird-exporter', 'nftables'], 'bird', catalog), { extensions: ['nftables'], dropped: ['prometheus-bird-exporter'] })
  assert.deepEqual(toggleExtension(['bird', 'prometheus-bird-exporter'], 'bird', false, catalog), {
    extensions: [],
    note: 'prometheus-bird-exporter unticked too: it requires bird.',
  })
  assert.deepEqual(toggleExtension(['bird', 'prometheus-bird-exporter'], 'prometheus-bird-exporter', false, catalog), { extensions: ['bird'], note: '' })
})
