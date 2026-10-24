import assert from 'node:assert/strict'
import { test } from 'node:test'
import { clusterNameError, domainsOf, health, memberDefaults, memberNameError, parseVars, pathError, varsText, versionChanges } from './clusters.js'

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
