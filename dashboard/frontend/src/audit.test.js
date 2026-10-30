import assert from 'node:assert/strict'
import { test } from 'node:test'
import { filtersRoute, fromLocalInput, readFilters, subjectGroups, subjectHref, subjectLabel, toLocalInput, viaLabel } from './audit.js'

test('the filters go round the hash', () => {
  const f = { user: 'rita', result: 'refused', subject: 'node:3f9c', q: 'haproxy config', from: '2026-10-29T13:00:00Z' }
  const route = filtersRoute(f)
  assert.equal(route, '/audit?user=rita&subject=node%3A3f9c&result=refused&from=2026-10-29T13%3A00%3A00Z&q=haproxy+config')
  assert.deepEqual(readFilters(route), f)
  assert.equal(filtersRoute({ user: '', kind: undefined }), '/audit')
  assert.deepEqual(readFilters('/audit'), {})
  // Unknown names are dropped.
  assert.deepEqual(readFilters('/audit?nope=1&method=POST'), { method: 'POST' })
})

test('a subject: its name, its page while it exists', () => {
  assert.equal(subjectLabel({ kind: 'node', id: '3f9c1234567890abcdef', name: 'edge-1' }), 'edge-1')
  assert.equal(subjectLabel({ kind: 'node', id: '3f9c1234567890abcdef' }), '3f9c1234…')
  assert.equal(subjectLabel({ kind: 'account', id: 'rita' }), 'rita')
  assert.equal(subjectHref({ kind: 'node', id: 'n1' }), '/nodes/n1/')
  assert.equal(subjectHref({ kind: 'node', id: 'n1', gone: true }), null)
  assert.equal(subjectHref({ kind: 'cluster', id: 'c1' }), '#/clusters/c1')
  assert.equal(subjectHref({ kind: 'machine', id: 'm1' }), null)
})

test("the subjects' picker: by kind, a shared link's kept", () => {
  const subjects = [
    { kind: 'machine', id: 'm1', name: 'edge-1' },
    { kind: 'node', id: 'n2', name: 'edge-2', gone: true },
    { kind: 'node', id: 'n1', name: 'edge-1' },
  ]
  const groups = subjectGroups(subjects, 'cluster:c9')
  assert.deepEqual(
    groups.map((g) => [g.kind, g.items.map((s) => s.id)]),
    [
      ['node', ['n2', 'n1']],
      ['machine', ['m1']],
      ['cluster', ['c9']],
    ],
  )
  assert.deepEqual(subjectGroups(undefined, ''), [])
})

test('how a request came', () => {
  assert.equal(viaLabel('session'), 'Session')
  assert.equal(viaLabel('token 7f3a'), 'API token 7f3a')
  assert.equal(viaLabel('sso kc'), 'sso kc')
})

test('times: RFC 3339 in the hash, local in the inputs', () => {
  const iso = '2026-10-29T13:05:00Z'
  assert.equal(fromLocalInput(toLocalInput(iso)), iso)
  assert.equal(toLocalInput(''), '')
  assert.equal(toLocalInput('nonsense'), '')
  assert.equal(fromLocalInput(''), '')
})
