// node --test (npm test)
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { listenAddress, listenMode, listenSaid, scrapeHost } from './listen.js'

test('the listen choices and the addresses they save', () => {
  assert.deepEqual(['auto', '', '10.0.0.5'].map(listenMode), ['auto', 'all', 'one'])
  assert.equal(listenAddress('auto', 'x'), 'auto')
  assert.equal(listenAddress('all', '10.0.0.5'), '')
  assert.equal(listenAddress('one', ' 10.0.0.5 '), '10.0.0.5')
  assert.deepEqual(['auto', '', 'fd00::1'].map(listenSaid), ['the main interface', 'every address', 'fd00::1'])
})

test('where Prometheus scrapes it', () => {
  assert.equal(scrapeHost('auto', ['10.0.2.15'], '10.0.2.15:9505'), '10.0.2.15')
  assert.equal(scrapeHost('auto', ['2001:db8::10', '10.0.2.15'], ''), '[2001:db8::10]')
  assert.equal(scrapeHost('', [], '[2001:db8::1]:9505'), '[2001:db8::1]')
  assert.equal(scrapeHost('192.0.2.7', undefined, '10.0.2.15:9505'), '192.0.2.7') // an older node: no effective addresses
  assert.equal(scrapeHost('auto', undefined, ''), '<node>')
})
