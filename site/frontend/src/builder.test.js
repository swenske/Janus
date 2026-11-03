import { test } from 'node:test'
import assert from 'node:assert/strict'
import { OLD_SLOT_BYTES, TESTER_FORMS, extensionRequestURL, imageWeight, selectablePlatform, testerIssueURL } from './builder.js'

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

test('a schematic weighs its base, its HAProxy branch and its extensions', () => {
  const MiB = 1024 * 1024
  const catalog = {
    slot_bytes: 128 * MiB,
    base_bytes: 12 * MiB,
    haproxy: [
      { name: '3.4', default: true, bytes: 3 * MiB },
      { name: '3.2', bytes: 2 * MiB },
    ],
    extensions: [
      { name: 'consul', bytes: 30 * MiB },
      { name: 'nftables', bytes: MiB },
    ],
  }
  assert.deepEqual(imageWeight(catalog, [], ''), { bytes: 15 * MiB, slot: 128 * MiB, fits: true, fitsOld: true })
  assert.equal(imageWeight(catalog, ['nftables'], '3.2').bytes, 15 * MiB)
  const heavy = imageWeight({ ...catalog, extensions: [{ name: 'big', bytes: 60 * MiB }] }, ['big'], '')
  assert.equal(heavy.fitsOld, false)
  assert.equal(heavy.fits, true)
  assert.equal(imageWeight({ ...catalog, slot_bytes: 64 * MiB, extensions: [{ name: 'big', bytes: 60 * MiB }] }, ['big'], '').fits, false)
  assert.equal(imageWeight({ ...catalog, base_bytes: 0 }, [], ''), null)
  assert.equal(imageWeight(null, [], ''), null)
  assert.equal(OLD_SLOT_BYTES, 64 * MiB)
})
