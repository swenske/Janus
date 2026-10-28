import assert from 'node:assert/strict'
import { test } from 'node:test'
import { behindLatest, clusterNameError, domainsOf, groupByCluster, health, memberDefaults, memberNameError, parseVars, pathError, releaseError, upgradeRows, varsText, versionChanges, versionOf } from './clusters.js'

test('a node gives its member name and address when it can', () => {
  assert.deepEqual(memberDefaults({ name: 'LB-1', address: '192.0.2.11:9505' }), { name: 'lb-1', address: '192.0.2.11' })
  assert.deepEqual(memberDefaults({ name: 'edge par 1', address: '[2001:db8::1]:9505' }), { name: 'edge-par-1', address: '2001:db8::1' })
  // A loopback - a NAT, a test - isn't where the members reach it.
  assert.equal(memberDefaults({ name: 'x', address: '127.0.0.1:19041' }).address, '')
  assert.equal(memberDefaults({ name: 'x', address: '[::1]:9505' }).address, '')
  assert.equal(memberDefaults({ name: '---', address: '' }).name, '')
})

test('names', () => {
  assert.equal(clusterNameError('edge'), '')
  assert.notEqual(clusterNameError('Edge Cluster'), '')
  assert.notEqual(clusterNameError(''), '')
  assert.equal(memberNameError('lb-1'), '')
  assert.notEqual(memberNameError('lb_1'), '')
})

test('a cluster’s health', () => {
  assert.equal(health({ busy: true, phase: 'applying version 2' }).label, 'applying version 2')
  assert.equal(health({ error: 'partially applied' }).tone, 'danger')
  assert.equal(health({ problems: ['a', 'b'] }).label, '2 to look at')
  assert.equal(health({ problems: [] }).tone, 'ok')
})

test('the domains a version configures', () => {
  assert.deepEqual(domainsOf(['keepalived.conf', 'haproxy.cfg', 'files/maps/x.map', 'files/errors/503.http']), ['haproxy', 'files', 'vrrp'])
  assert.deepEqual(domainsOf([]), [])
})

test('what a version changes', () => {
  const from = { 'haproxy.cfg': 'global\n    maxconn 100\n', 'files/old.map': 'a\n' }
  const to = { 'haproxy.cfg': 'global\n    maxconn 200\n', 'keepalived.conf': 'vrrp\n' }
  const c = versionChanges(from, to)
  assert.deepEqual(
    c.map((x) => [x.path, x.kind]),
    [
      ['files/old.map', 'removed'],
      ['haproxy.cfg', 'changed'],
      ['keepalived.conf', 'added'],
    ],
  )
  const cfg = c.find((x) => x.path === 'haproxy.cfg').diff
  assert.ok(cfg.some((d) => d.op === '-' && d.text.includes('100')))
  assert.ok(cfg.some((d) => d.op === '+' && d.text.includes('200')))
  assert.deepEqual(versionChanges(to, to), [])
})

test('variables', () => {
  const v = parseVars('priority = 150\n# a comment\n\nrouter_id=192.0.2.1')
  assert.deepEqual(v, { priority: '150', router_id: '192.0.2.1' })
  assert.equal(varsText(v), 'priority=150\nrouter_id=192.0.2.1')
  assert.throws(() => parseVars('priority'), /key=value/)
  assert.throws(() => parseVars('1st=x'), /letters/)
})

test('a version’s file paths', () => {
  for (const ok of ['haproxy.cfg', 'bird.conf', 'files/maps/hosts.map', 'files/errors.http']) assert.equal(pathError(ok), '', ok)
  for (const bad of ['etc/passwd', 'files/../x', 'files/a/b/c', 'haproxy.conf']) assert.notEqual(pathError(bad), '', bad)
})

test('the Nodes page groups a cluster\'s members, the others after', () => {
  const nodes = [{ id: 'a' }, { id: 'b' }, { id: 'c' }, { id: 'd' }]
  const edge = { id: 'e', members: [{ node_id: 'c' }, { node_id: 'a' }] }
  const core = { id: 'k', members: [{ node_id: 'x' }] } // no member among the nodes: no group
  const g = groupByCluster(nodes, [edge, core])
  assert.deepEqual(
    g.map((x) => [x.cluster?.id ?? null, x.nodes.map((n) => n.id)]),
    [
      ['e', ['a', 'c']],
      [null, ['b', 'd']],
    ],
  )
  // Every node in a cluster: no empty group of the others.
  assert.deepEqual(groupByCluster([{ id: 'a' }], [edge]).map((x) => x.cluster?.id ?? null), ['e'])
  // No cluster: one group of every node.
  assert.deepEqual(groupByCluster(nodes, []), [{ cluster: null, nodes }])
  assert.deepEqual(groupByCluster([], null), [{ cluster: null, nodes: [] }])
})

test("a cluster's version, against the one it wants", () => {
  assert.deepEqual(versionOf({ desired: 2, members: [{ state: { applied: 2 } }, { state: { applied: 2 } }] }), { label: 'v2', tone: '' })
  assert.deepEqual(versionOf({ desired: 2, members: [{ state: { applied: 1 } }, { state: { applied: 2 } }] }), { label: 'v1/v2 → v2', tone: 'warn' })
  assert.deepEqual(versionOf({ desired: 1, members: [{}] }), { label: 'none → v1', tone: 'warn' })
  assert.equal(versionOf({ members: [] }), '')
})

test('an update in place: the release typed, the members on it, those behind', () => {
  assert.equal(releaseError('v2026.10.29'), '')
  assert.equal(releaseError('v2026.10.29-3'), '')
  assert.notEqual(releaseError('2026.10.29'), '')
  assert.notEqual(releaseError('v2026.10.29-3-g1234abc'), '')
  const c = {
    members: [
      { name: 'lb-1', state: { release: 'v2026.10.28-2' } },
      { name: 'lb-2', state: { release: 'v2026.10.29', stage: 'rebooting' } },
      { name: 'lb-3', state: {} },
    ],
  }
  assert.deepEqual(upgradeRows(c, 'v2026.10.29'), [
    { name: 'lb-1', release: 'v2026.10.28-2', onTarget: false, stage: '' },
    { name: 'lb-2', release: 'v2026.10.29', onTarget: true, stage: 'rebooting' },
    { name: 'lb-3', release: '', onTarget: false, stage: '' },
  ])
  assert.deepEqual(behindLatest(c, 'v2026.10.29'), ['lb-1'])
  assert.deepEqual(behindLatest(c, ''), [])
})
