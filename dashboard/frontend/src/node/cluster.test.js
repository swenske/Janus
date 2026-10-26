// node --test (npm test)
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { LOCKED_WRITES, clusterLink, lockedDomain, vrrpRole } from './cluster.js'

const member = { member: true, id: 'c1', name: 'edge', member_name: 'edge-1', managed: ['haproxy', 'files', 'vrrp'] }

test('a write to what the cluster manages here is locked, the rest is not', () => {
  assert.equal(lockedDomain(member, 'HAProxyService/ApplyConfig'), 'haproxy')
  assert.equal(lockedDomain(member, 'HAProxyService/CertificateUpload'), 'files')
  assert.equal(lockedDomain(member, 'NetworkService/VRRPApplyConfig'), 'vrrp')
  assert.equal(lockedDomain(member, 'NetworkService/BGPApplyConfig'), '') // BGP isn't managed
  assert.equal(lockedDomain(member, 'HAProxyService/GetConfig'), '') // a read
  assert.equal(lockedDomain(member, 'HAProxyService/ServerSetState'), '') // runtime stays the node's
  assert.equal(lockedDomain(null, 'HAProxyService/ApplyConfig'), '')
  assert.equal(lockedDomain({ member: false, managed: ['haproxy'] }, 'HAProxyService/ApplyConfig'), '')
  // A released domain is no longer in managed.
  assert.equal(lockedDomain({ ...member, managed: ['files', 'vrrp'] }, 'HAProxyService/ApplyConfig'), '')
})

test('every locked write names a domain the node knows', () => {
  for (const d of Object.values(LOCKED_WRITES)) assert.ok(['haproxy', 'files', 'vrrp', 'bgp', 'firewall'].includes(d), d)
})

test("the cluster's page is the Controller's, not a janusctl cluster's", () => {
  assert.equal(clusterLink(member), '/#/clusters/c1')
  assert.equal(clusterLink({ ...member, advisory: true }), '')
  assert.equal(clusterLink(null), '')
})

test("the node's VRRP role", () => {
  assert.equal(vrrpRole(null), '')
  assert.equal(vrrpRole({ instances: [] }), '')
  assert.equal(vrrpRole({ instances: [{ state: 'master' }] }), 'MASTER')
  assert.equal(vrrpRole({ instances: [{ state: 'BACKUP' }, { state: 'MASTER' }] }), 'MASTER')
  assert.equal(vrrpRole({ instances: [{ state: 'BACKUP' }, { state: 'BACKUP' }] }), 'BACKUP')
  assert.equal(vrrpRole({ instances: [{ state: 'BACKUP' }, { state: 'FAULT' }] }), 'FAULT')
})
