import assert from 'node:assert/strict'
import { test } from 'node:test'
import { counts, dirty, draftOf, never, settingsOf, stateTone, trustMode } from './consul.js'

const saved = {
  enabled: true,
  address: 'https://consul.example',
  tls: { ca_pem: '-----BEGIN CERTIFICATE-----\nAA\n-----END CERTIFICATE-----\n' },
  has_token: true,
  rpc_ca_pem: '',
  selector: 'dc=lgs',
  services: { janus: true, exporters: true, frontends: false, urls: true },
  node_meta_from_labels: true,
}

test('the saved settings round-trip unchanged', () => {
  const d = draftOf(saved)
  assert.equal(d.trust, 'ca')
  assert.equal(d.token, '')
  assert.equal(dirty(d, saved), false)
  const s = settingsOf(d)
  // The token left out: the saved one is kept.
  assert.equal(s.token, undefined)
  assert.equal(s.tls.ca_pem, saved.tls.ca_pem)
  assert.equal(s.tls.fingerprint, undefined)
  assert.deepEqual(s.services, saved.services)
})

test('a change is dirty, a token typed is sent', () => {
  const d = { ...draftOf(saved), token: ' 11111111-2222-3333-4444-555555555555 ' }
  assert.equal(dirty(d, saved), true)
  assert.equal(settingsOf(d).token, '11111111-2222-3333-4444-555555555555')
  assert.equal(dirty({ ...draftOf(saved), selector: 'dc=lgs, ' }, saved), false)
  assert.equal(dirty({ ...draftOf(saved), address: 'https://consul.example/' }, saved), false)
  assert.equal(settingsOf({ ...draftOf(saved), selector: ' dc=lgs , room=a ' }).selector, 'dc=lgs,room=a')
})

test('trust: a CA, a fingerprint or the system store - one at a time', () => {
  assert.equal(trustMode({}), 'system')
  assert.equal(trustMode({ fingerprint: 'AB:CD' }), 'fingerprint')
  const d = { ...draftOf(saved), trust: 'fingerprint', fingerprint: 'AB:CD' }
  const s = settingsOf(d)
  assert.equal(s.tls.ca_pem, undefined)
  assert.equal(s.tls.fingerprint, 'AB:CD')
  assert.deepEqual(settingsOf({ ...draftOf(saved), trust: 'system' }).tls, { server_name: undefined })
})

test('the defaults: nothing saved yet', () => {
  const d = draftOf(undefined)
  assert.equal(d.enabled, false)
  assert.equal(d.trust, 'system')
  assert.equal(settingsOf(d).address, '')
})

test('nodes summed up, states toned', () => {
  const c = counts([
    { state: 'applied', member: 'alive', urls: 3 },
    { state: 'applied', member: 'failed', urls: 1 },
    { state: 'error' },
    { state: 'no extension' },
  ])
  assert.deepEqual(c, { applied: 2, error: 1, alive: 1, failed: 1, urls: 4 })
  assert.equal(stateTone('applied'), 'ok')
  assert.equal(stateTone('error'), 'danger')
  assert.equal(stateTone('not picked'), '')
  assert.equal(never('0001-01-01T00:00:00Z'), true)
  assert.equal(never('2026-10-29T08:00:00Z'), false)
})
