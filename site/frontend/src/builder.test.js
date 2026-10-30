import { test } from 'node:test'
import assert from 'node:assert/strict'
import { TESTER_FORMS, extensionRequestURL, selectablePlatform, testerIssueURL } from './builder.js'

const platforms = [
  { id: 'proxmox', arch: 'amd64' },
  { id: 'vmware', arch: 'amd64', target: 'vmware', suspended: true },
  { id: 'rpi4', arch: 'arm64', target: 'raspberry-pi', suspended: true },
]

test('a suspended or unknown platform is never selected', () => {
  assert.equal(selectablePlatform(platforms, 'proxmox'), 'proxmox')
  assert.equal(selectablePlatform(platforms, 'vmware'), '')
  assert.equal(selectablePlatform(platforms, 'rpi4'), '')
  assert.equal(selectablePlatform(platforms, 'nope'), '')
  assert.equal(selectablePlatform(platforms, ''), '')
  assert.equal(selectablePlatform(null, 'proxmox'), '')
})

test('each suspended target opens its own issue form', () => {
  assert.equal(testerIssueURL('vmware'), 'https://github.com/swenske/Janus/issues/new?template=need-tester-vmware.yml')
  assert.equal(testerIssueURL('raspberry-pi'), 'https://github.com/swenske/Janus/issues/new?template=need-tester-raspberry-pi.yml')
  assert.deepEqual(Object.keys(TESTER_FORMS), ['vmware', 'baremetal', 'raspberry-pi'])
  assert.equal(testerIssueURL('other'), 'https://github.com/swenske/Janus/issues/new/choose')
  assert.ok(!testerIssueURL('vmware').includes('labels='))
})

test('the extension request form', () => {
  assert.equal(extensionRequestURL(), 'https://github.com/swenske/Janus/issues/new?template=extension-request.yml')
})
