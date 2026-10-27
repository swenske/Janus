// node --test (npm test)
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { LOCKED_WRITES, bgpState, clusterLink, lockedDomain, routerRoles, vrrpRole } from './cluster.js'

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

const bgp = (protocols, state = 'running') => ({ state, protocols })
const session = (name, bgpState, exported) => ({ name, protocol: 'BGP', bgp_state: bgpState, channels: [{ name: 'ipv4', exported }] })

test("the node's own BGP state", () => {
  assert.equal(bgpState(null).state, '')
  assert.equal(bgpState(bgp([], 'waiting')).state, '') // no bird.conf
  assert.equal(bgpState(bgp([{ name: 'device1', protocol: 'Device' }])).state, '') // no session
  assert.equal(bgpState(bgp([session('up', 'Established', 1)])).state, 'announcing')
  assert.equal(bgpState(bgp([session('up', 'Active', 0)])).state, 'down')
  // Up but exporting nothing: the haproxy_* routes held down (HAProxy, a drain).
  const held = bgpState(bgp([session('up', 'Established', 0), { name: 'haproxy_anycast', protocol: 'Static', held_down: true }]))
  assert.equal(held.state, 'withdrawn')
  assert.equal(held.held, true)
  // One family's session down: still announcing, partially.
  const half = bgpState(bgp([session('v4', 'Established', 1), session('v6', 'Active', 0)]))
  assert.equal(half.state, 'announcing')
  assert.equal(half.partial, true)
})

test("the router's roles for this member", () => {
  assert.equal(routerRoles(null).label, '')
  assert.equal(routerRoles({ available: false }).label, '')
  const both = { available: true, roles: [{ prefix: '192.0.2.10/32', role: 'multipath' }, { prefix: '2001:db8::10/128', role: 'multipath' }] }
  assert.equal(routerRoles(both).label, 'multipath')
  const mixed = { available: true, roles: [{ prefix: '192.0.2.10/32', role: 'master' }, { prefix: '2001:db8::10/128', role: 'backup' }] }
  assert.equal(routerRoles(mixed).label, 'master (192.0.2.10/32), backup (2001:db8::10/128)')
})

