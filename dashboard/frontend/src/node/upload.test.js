import { test } from 'node:test'
import assert from 'node:assert/strict'
import { ndjsonLines, uploadPosition } from './upload.js'

const files = [
  { name: 'rootfs.squashfs', size: 1000 },
  { name: 'rootfs.verity', size: 100 },
  { name: 'uki-a.efi', size: 300 },
  { name: 'uki-b.efi', size: 300 },
]

test('uploadPosition follows the files in their order', () => {
  assert.deepEqual(uploadPosition(files, 0), { index: 0, name: 'rootfs.squashfs', bytes: 0, size: 1000 })
  assert.deepEqual(uploadPosition(files, 999), { index: 0, name: 'rootfs.squashfs', bytes: 999, size: 1000 })
  assert.deepEqual(uploadPosition(files, 1000), { index: 1, name: 'rootfs.verity', bytes: 0, size: 100 })
  assert.deepEqual(uploadPosition(files, 1150), { index: 2, name: 'uki-a.efi', bytes: 50, size: 300 })
  // The form's last fields and the multipart headers: the last file, whole.
  assert.deepEqual(uploadPosition(files, 1700), { index: 3, name: 'uki-b.efi', bytes: 300, size: 300 })
  assert.deepEqual(uploadPosition(files, 2100), { index: 3, name: 'uki-b.efi', bytes: 300, size: 300 })
  assert.equal(uploadPosition([], 10), null)
})

test('ndjsonLines reads the lines as the text grows', () => {
  const read = ndjsonLines()
  assert.deepEqual(read(''), [])
  assert.deepEqual(read('{"stage":"upl'), [])
  assert.deepEqual(read('{"stage":"upload"}\n{"stage":'), [{ stage: 'upload' }])
  assert.deepEqual(read('{"stage":"upload"}\n{"stage":"write","percent":40}\n\n'), [{ stage: 'write', percent: 40 }])
  assert.deepEqual(read('{"stage":"upload"}\n{"stage":"write","percent":40}\n\n{"stage":"done"}', true), [{ stage: 'done' }])
  assert.deepEqual(read('{"stage":"upload"}\n{"stage":"write","percent":40}\n\n{"stage":"done"}', true), [])
})
