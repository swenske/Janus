// node --test src/ (npm test)
import assert from 'node:assert/strict'
import { test } from 'node:test'
import { ansiSegments, color256 } from './ansi.js'

const ESC = '\x1b'

test('plain text and line endings', () => {
  assert.deepEqual(ansiSegments('boot\r\nok\rdone\n'), [{ text: 'boot\nokdone\n', style: null }])
})

test("the Janus banner's colors", () => {
  const banner = `  ██${ESC}[38;5;166m▐▌${ESC}[0m██   ${ESC}[1mJ A N U S${ESC}[0m\n${ESC}[2malpha${ESC}[0m`
  assert.deepEqual(ansiSegments(banner), [
    { text: '  ██', style: null },
    { text: '▐▌', style: { color: 'rgb(215,95,0)' } },
    { text: '██   ', style: null },
    { text: 'J A N U S', style: { fontWeight: 700 } },
    { text: '\n', style: null },
    { text: 'alpha', style: { opacity: 0.6 } },
  ])
})

test('16, 256 and true colors, backgrounds, resets', () => {
  const segs = ansiSegments(`${ESC}[31mred${ESC}[39m ${ESC}[1;92;44mbright${ESC}[22;49m ${ESC}[38;2;1;2;3mrgb${ESC}[m.`)
  assert.deepEqual(segs, [
    { text: 'red', style: { color: '#e06c75' } },
    { text: ' ', style: null },
    { text: 'bright', style: { color: '#b5e890', backgroundColor: '#61afef', fontWeight: 700 } },
    { text: ' ', style: { color: '#b5e890' } },
    { text: 'rgb', style: { color: 'rgb(1,2,3)' } },
    { text: '.', style: null },
  ])
  assert.equal(color256(16), 'rgb(0,0,0)')
  assert.equal(color256(231), 'rgb(255,255,255)')
  assert.equal(color256(244), 'rgb(128,128,128)')
})

test('other sequences dropped, a cut one hidden until complete', () => {
  assert.deepEqual(ansiSegments(`${ESC}[2J${ESC}[H${ESC}]0;title\x07screen${ESC}[38;5`), [{ text: 'screen', style: null }])
  assert.deepEqual(ansiSegments(`a${ESC}`), [{ text: 'a', style: null }])
})
