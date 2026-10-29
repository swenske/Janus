import assert from 'node:assert/strict'
import { test } from 'node:test'
import { address, agentHint, checksText, counts, keyMeta, kindOf, maintenanceReason, matches } from './consulServices.js'

const janus = { id: 'janus:janus', name: 'janus', address: '10.0.0.5', port: 9505, tags: ['janus', 'grpc'], meta: { version: 'v1' }, managed: true }
const exporter = { id: 'janus:janus-exporter', name: 'janus-exporter', address: '10.0.0.5', port: 10056, tags: ['janus', 'prometheus'], meta: { metrics_path: '/metrics', scheme: 'http' } }
const haexp = { id: 'janus:haproxy-exporter:prom', name: 'haproxy-exporter', port: 8405, tags: ['haproxy', 'prometheus'], meta: { metrics_path: '/metrics', frontend: 'prom' } }
const frontend = { id: 'janus:haproxy:fe_https', name: 'haproxy-fe-https', address: '2001:db8::1', port: 443, tags: ['haproxy', 'http', 'tls'], meta: { frontend: 'fe_https' } }
const url = {
  id: 'janus:url:fe_https:www.example.com',
  name: 'haproxy-url',
  port: 443,
  tags: ['haproxy', 'url', 'https'],
  meta: { url: 'https://www.example.com', frontend: 'fe_https' },
  status: 'maintenance',
  checks: [{ id: '_service_maintenance:janus:url:fe_https:www.example.com', name: 'Service Maintenance Mode', status: 'critical', output: 'drained' }],
}
const web = { id: 'web', name: 'web', port: 8080, tags: [], meta: {} }

test('kinds', () => {
  assert.equal(kindOf(janus), 'other')
  assert.equal(kindOf(exporter), 'exporter')
  assert.equal(kindOf(haexp), 'exporter') // tag prometheus first: not a frontend
  assert.equal(kindOf(frontend), 'frontend')
  assert.equal(kindOf(url), 'url')
  assert.equal(kindOf(web), 'other')
  assert.deepEqual(counts([janus, exporter, haexp, frontend, url, web]), { all: 6, frontend: 1, url: 1, exporter: 2, other: 2 })
})

test('filtering', () => {
  assert.equal(matches(url, 'url'), true)
  assert.equal(matches(url, 'frontend'), false)
  assert.equal(matches(url, 'all'), true)
  assert.equal(matches(haexp, 'exporter'), true)
})

test('meta, address, checks', () => {
  assert.equal(keyMeta(url), 'https://www.example.com')
  assert.equal(keyMeta(exporter), 'http /metrics')
  assert.equal(keyMeta(haexp), 'http /metrics')
  assert.equal(keyMeta(frontend), 'fe_https')
  assert.equal(keyMeta(janus), '')
  assert.equal(address(frontend), '[2001:db8::1]:443')
  assert.equal(address(web), ':8080')
  assert.equal(address({ address: '', port: 0 }), '-')
  assert.equal(maintenanceReason(url), 'drained')
  assert.equal(maintenanceReason(frontend), '')
  assert.equal(checksText(url), 'Service Maintenance Mode: critical - drained')
})

test('what to say instead', () => {
  assert.equal(agentHint({ state: 'running', service_state: 'running' }), '')
  assert.match(agentHint({ state: 'stopped', service_state: 'waiting' }), /isn't running \(waiting\)/)
  assert.match(agentHint({ state: 'running', service_state: 'running', error: "the agent's HTTP API refuses anonymous reads (ACLs): Permission denied" }), /anonymous reads/)
  // Managed: the token is the layer's - an error isn't about anonymity.
  assert.equal(agentHint({ state: 'running', service_state: 'running', managed: true, error: 'refuses the managed token' }), '')
  assert.equal(agentHint({ state: 'not_enabled' }), '')
})
