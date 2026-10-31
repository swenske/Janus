import assert from 'node:assert/strict'
import { test } from 'node:test'
import { fleetAlert, needsKit, renewalProgress } from './fleetAlert.js'

const DAY = 24 * 3600 * 1000
const now = Date.parse('2028-05-01T00:00:00Z')
const fleet = (days, renewFromDays = 180) => ({
  state: 'ready',
  issuing_not_after: new Date(now + days * DAY + 3600 * 1000).toISOString(),
  renew_from: new Date(now + (days - renewFromDays) * DAY).toISOString(),
})

test('the issuing CA: nothing until 180 days before its end, then the card, then the badge, then danger', () => {
  assert.equal(fleetAlert(fleet(200), now), null)
  const card = fleetAlert(fleet(120), now)
  assert.equal(card.level, 'warn')
  assert.equal(card.badge, null)
  assert.match(card.text, /120 days.*recovery kit/)
  assert.deepEqual([fleetAlert(fleet(45), now).level, fleetAlert(fleet(45), now).badge], ['warn', 'Fleet CA: 45 days left'])
  assert.deepEqual([fleetAlert(fleet(10), now).level, fleetAlert(fleet(10), now).badge], ['danger', 'Fleet CA: 10 days left'])
  assert.equal(fleetAlert(fleet(-2), now).badge, 'Fleet CA expired')
  assert.match(fleetAlert(fleet(-2), now).text, /reaches no node/)
})

test('no fleet, or not ready: nothing', () => {
  assert.equal(fleetAlert(null, now), null)
  assert.equal(fleetAlert({ state: 'pending', issuing_not_after: '2020-01-01T00:00:00Z' }, now), null)
})

test('the nodes that need the kit, and a renewal waiting for its nodes', () => {
  const f = {
    nodes: { a: { state: 'trusted' }, b: { state: 'needs-kit', error: 'give the recovery kit' } },
    renewal: { version: 7, switch_by: '2028-09-01T00:00:00Z' },
    nodes_behind: ['beta'],
  }
  assert.deepEqual(needsKit(f), [{ id: 'b', error: 'give the recovery kit' }])
  assert.deepEqual(renewalProgress(f), { version: 7, behind: ['beta'], switchBy: '2028-09-01T00:00:00Z' })
  assert.equal(renewalProgress({ renewal: { version: 7, switched: '2028-05-01T00:00:00Z' } }), null)
  assert.equal(renewalProgress({}), null)
})
