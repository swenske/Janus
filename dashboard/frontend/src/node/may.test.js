// node --test (npm test)
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { changesSomething, mayFor } from './may.js'

const me = {
  may: ['HAProxyService/ApplyConfig', 'HAProxyService/GetConfig', 'HAProxyService/Reload', 'NetworkService/VRRPApplyConfig', 'SystemService/Logs', 'SystemService/Probe'],
  reads: ['HAProxyService/GetConfig', 'SystemService/Logs', 'SystemService/Probe'],
}
const member = { member: true, managed: ['haproxy'] }

test('read-only: what reads stays, what changes goes, validate stays', () => {
  const editing = mayFor({ ...me, cluster: null, readOnly: false })
  const ro = mayFor({ ...me, cluster: null, readOnly: true })
  assert.equal(editing('HAProxyService/Reload'), true)
  assert.equal(ro('HAProxyService/Reload'), false)
  assert.equal(ro('SystemService/Logs'), true) // an operator's, yet a read
  assert.equal(ro('SystemService/Probe'), true) // diagnosis
  assert.equal(ro('NetworkService/VRRPApplyConfig'), false)
  assert.equal(ro('NetworkService/VRRPApplyConfig', 'validate'), true) // its Check button
  assert.equal(ro('LifecycleService/Upgrade', 'validate'), false) // never the role's
})

test('a cluster lock and the role decide before the switch', () => {
  const ro = mayFor({ ...me, cluster: member, readOnly: true })
  assert.equal(ro('HAProxyService/ApplyConfig', 'validate'), false)
  assert.equal(mayFor({ ...me, cluster: member, readOnly: false })('HAProxyService/ApplyConfig'), false)
  assert.equal(mayFor({ may: undefined, reads: undefined, cluster: null, readOnly: false })('SystemService/Logs'), false) // nothing until /api/me says
})

test('no switch when there is nothing to hide', () => {
  assert.equal(changesSomething({ ...me, cluster: null }), true)
  assert.equal(changesSomething({ may: me.reads, reads: me.reads, cluster: null }), false) // a reader
  assert.equal(changesSomething({ may: ['HAProxyService/ApplyConfig', 'HAProxyService/GetConfig'], reads: ['HAProxyService/GetConfig'], cluster: member }), false) // all locked
  assert.equal(changesSomething({ may: me.may, reads: undefined, cluster: null }), false) // a Controller that doesn't say
})
