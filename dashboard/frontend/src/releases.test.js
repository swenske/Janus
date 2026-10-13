import assert from 'node:assert/strict'
import test from 'node:test'
import { compareRelease } from './releases.js'

test('compareRelease: a release against the version a node runs', () => {
  assert.equal(compareRelease('v2026.10.13-4', 'v2026.10.12'), 1)
  assert.equal(compareRelease('v2026.10.12', 'v2026.10.13-2'), -1)
  assert.equal(compareRelease('v2026.10.13', 'v2026.10.13-2'), -1)
  assert.equal(compareRelease('v2026.10.13-2', 'v2026.10.13-2'), 0)
  assert.equal(compareRelease('v2026.10.13-10', 'v2026.10.13-9'), 1)
  // A build after a release counts as that release.
  assert.equal(compareRelease('v2026.10.13-4', 'v2026.10.13-4-12-g550e69a-dirty'), 0)
  // Nothing to compare.
  assert.equal(compareRelease('v2026.10.13-4', 'dev'), null)
  assert.equal(compareRelease('v2026.10.13-4-3-gabc', 'v2026.10.12'), null)
  assert.equal(compareRelease('', 'v2026.10.12'), null)
  assert.equal(compareRelease('v2026.10.13', undefined), null)
})
