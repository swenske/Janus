import { Fragment, useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { Badge } from '@shared/ui.jsx'
import { DOCS, REPO } from './api.js'
import { Link } from './App.jsx'
import './tour.css'
import wordmark from '@brand/logo/janus-logo-mono-adaptive.svg'
// Inlined, not an <img>: the hero animates its bar and its two faces.
import symbol from '@brand/symbole/janus-symbole.svg?raw'
import nodesDark from '@screens/controller-nodes-dark.webp'
import nodesLight from '@screens/controller-nodes-light.webp'
import configDark from '@screens/node-haproxy-config-dark.webp'
import configLight from '@screens/node-haproxy-config-light.webp'
import overviewDark from '@screens/node-overview-dark.webp'
import overviewLight from '@screens/node-overview-light.webp'
import tuiDark from '@screens/janusctl-tui-dark.webp'
import tuiLight from '@screens/janusctl-tui-light.webp'

// The feature tour (/tour): fourteen scenes played one after the other,
// each a set of CSS animations timed from the moment it comes on (--d:
// appear, --v: vanish, in seconds into the scene - tour.css), a
// transport bar, and a funk groove made live with the Web Audio API,
// off until asked for. With reduced motion: no autoplay, and every scene
// readable at rest.

const reducedMotion = () => window.matchMedia?.('(prefers-reduced-motion: reduce)').matches ?? false

/* ---------- Background music: a soft funk groove, generated live with the Web Audio API ---------- */
function createMusic() {
  let ctx, master, bus, delayIn, noise
  let timer = null
  let offTimer = null
  let step = 0
  let nextTime = 0
  const BPM = 104
  const S16 = 60 / BPM / 4
  const SWING = 0.14

  // Dm9 | G13 | Dm9 | Em7 - A7, keys voiced as rootless chords
  const KEYS = [
    [53, 57, 60, 64],
    [53, 59, 64, 69],
    [53, 57, 60, 64],
    [50, 55, 59, 62],
  ]
  const KEYS_B = [null, null, null, [55, 61, 64, 67]] // A7 on the second half of bar 4
  // bass: step -> [midi, length in 16ths, ghost]
  const BASS = [
    { 0: [38, 3], 3: [38, 1, 1], 4: [50, 1], 6: [48, 1], 7: [50, 1], 10: [38, 2], 12: [45, 1], 14: [41, 1], 15: [42, 1] },
    { 0: [43, 3], 3: [43, 1, 1], 4: [55, 1], 6: [53, 1], 7: [55, 1], 10: [43, 2], 12: [50, 1], 14: [47, 1], 15: [49, 1] },
    { 0: [38, 3], 3: [38, 1, 1], 4: [50, 1], 6: [48, 1], 7: [50, 1], 10: [38, 2], 12: [45, 1], 13: [48, 1], 14: [50, 1] },
    { 0: [40, 2], 3: [52, 1], 4: [40, 1, 1], 6: [43, 2], 8: [45, 2], 11: [57, 1], 12: [45, 1], 14: [48, 1], 15: [49, 1] },
  ]
  const STABS = { 2: 0.9, 6: 0.7, 9: 0.8, 14: 0.6 }
  const SCRATCH = [0.25, 0.12, 0.5, 0.12, 1, 0.12, 0.3, 0.2, 0.25, 0.12, 0.5, 0.15, 1, 0.12, 0.35, 0.2]
  const HATS = [0.05, 0.018, 0.034, 0.02, 0.05, 0.018, 0.034, 0.024]
  const LICK = [{ 0: 81, 2: 79, 3: 77, 6: 74, 10: 77, 11: 79 }, null, { 0: 84, 2: 81, 4: 79, 5: 81, 8: 77, 10: 74 }, null]

  const hz = (m) => 440 * Math.pow(2, (m - 69) / 12)

  function init() {
    const AC = window.AudioContext || window.webkitAudioContext
    ctx = new AC()
    const comp = ctx.createDynamicsCompressor()
    comp.threshold.value = -20
    comp.ratio.value = 3
    comp.attack.value = 0.01
    comp.release.value = 0.2
    master = ctx.createGain()
    master.gain.value = 0
    // a gentle top-end roll-off keeps the whole mix soft
    const tone = ctx.createBiquadFilter()
    tone.type = 'lowpass'
    tone.frequency.value = 7000
    bus = ctx.createGain()
    bus.gain.value = 1
    bus.connect(tone)
    tone.connect(master)
    master.connect(comp)
    comp.connect(ctx.destination)
    // eighth-note echo for keys and the lick
    delayIn = ctx.createGain()
    delayIn.gain.value = 0.35
    const d = ctx.createDelay(1)
    d.delayTime.value = S16 * 3
    const fb = ctx.createGain()
    fb.gain.value = 0.3
    const lp = ctx.createBiquadFilter()
    lp.type = 'lowpass'
    lp.frequency.value = 1800
    delayIn.connect(d)
    d.connect(lp)
    lp.connect(fb)
    fb.connect(d)
    lp.connect(bus)
    noise = ctx.createBuffer(1, ctx.sampleRate * 0.5, ctx.sampleRate)
    const ch = noise.getChannelData(0)
    for (let i = 0; i < ch.length; i++) ch[i] = Math.random() * 2 - 1
  }

  function env(g, t, a, peak, hold, r) {
    g.gain.setValueAtTime(0.0001, t)
    g.gain.exponentialRampToValueAtTime(peak, t + a)
    g.gain.setValueAtTime(peak, t + a + hold)
    g.gain.exponentialRampToValueAtTime(0.0001, t + a + hold + r)
  }
  function osc(type, f, t, end, dest, gain, detune) {
    const o = ctx.createOscillator()
    o.type = type
    o.frequency.value = f
    if (detune) o.detune.value = detune
    const g = ctx.createGain()
    g.gain.value = gain
    o.connect(g)
    g.connect(dest)
    o.start(t)
    o.stop(end)
  }

  // round synth bass with a short filter "pluck"
  function bass(m, t, len, ghost) {
    const dur = len * S16
    const f = ctx.createBiquadFilter()
    f.type = 'lowpass'
    f.Q.value = ghost ? 2 : 7
    f.frequency.setValueAtTime(ghost ? 500 : 1300, t)
    f.frequency.exponentialRampToValueAtTime(240, t + 0.16)
    const g = ctx.createGain()
    env(g, t, 0.006, ghost ? 0.06 : 0.2, Math.max(dur * 0.55, 0.03), 0.07)
    f.connect(g)
    g.connect(bus)
    osc('sawtooth', hz(m), t, t + dur + 0.2, f, 0.6)
    osc('sine', hz(m), t, t + dur + 0.2, g, ghost ? 0.2 : 0.55)
  }
  // electric-piano stab: sine body, octave shimmer, soft bell attack
  function stab(chord, t, vel) {
    const g = ctx.createGain()
    env(g, t, 0.004, 0.05 * vel, 0.06, 0.34)
    const f = ctx.createBiquadFilter()
    f.type = 'lowpass'
    f.frequency.value = 2400
    f.connect(g)
    g.connect(bus)
    g.connect(delayIn)
    for (const m of chord) {
      osc('sine', hz(m), t, t + 0.5, f, 0.5)
      osc('triangle', hz(m) * 2, t, t + 0.5, f, 0.12, 4)
    }
  }
  // muted guitar "chicken scratch": noise through a moving band-pass, accents get a wah sweep
  function scratch(t, vel) {
    const s = ctx.createBufferSource()
    s.buffer = noise
    const bp = ctx.createBiquadFilter()
    bp.type = 'bandpass'
    bp.Q.value = 4
    if (vel > 0.9) {
      bp.frequency.setValueAtTime(700, t)
      bp.frequency.exponentialRampToValueAtTime(2200, t + 0.08)
    } else bp.frequency.value = 1700
    const g = ctx.createGain()
    env(g, t, 0.002, 0.07 * vel, 0, vel > 0.9 ? 0.09 : 0.035)
    s.connect(bp)
    bp.connect(g)
    g.connect(bus)
    s.start(t, Math.random() * 0.3)
    s.stop(t + 0.15)
  }
  function lick(m, t) {
    const g = ctx.createGain()
    env(g, t, 0.01, 0.04, 0.08, 0.25)
    const lfo = ctx.createOscillator()
    lfo.frequency.value = 5.5
    const lg = ctx.createGain()
    lg.gain.value = 9
    const o = ctx.createOscillator()
    o.type = 'triangle'
    o.frequency.value = hz(m)
    lfo.connect(lg)
    lg.connect(o.detune)
    const f = ctx.createBiquadFilter()
    f.type = 'lowpass'
    f.frequency.value = 2200
    o.connect(f)
    f.connect(g)
    g.connect(bus)
    g.connect(delayIn)
    o.start(t)
    lfo.start(t)
    o.stop(t + 0.45)
    lfo.stop(t + 0.45)
  }
  function kick(t, v) {
    const o = ctx.createOscillator()
    o.type = 'sine'
    o.frequency.setValueAtTime(115, t)
    o.frequency.exponentialRampToValueAtTime(45, t + 0.12)
    const g = ctx.createGain()
    env(g, t, 0.003, 0.5 * v, 0.02, 0.24)
    o.connect(g)
    g.connect(bus)
    o.start(t)
    o.stop(t + 0.35)
  }
  function snare(t, v) {
    const s = ctx.createBufferSource()
    s.buffer = noise
    const bp = ctx.createBiquadFilter()
    bp.type = 'bandpass'
    bp.frequency.value = 2000
    bp.Q.value = 0.8
    const g = ctx.createGain()
    env(g, t, 0.002, 0.13 * v, 0.01, 0.13)
    s.connect(bp)
    bp.connect(g)
    g.connect(bus)
    s.start(t)
    s.stop(t + 0.2)
    const o = ctx.createOscillator()
    o.type = 'triangle'
    o.frequency.setValueAtTime(200, t)
    o.frequency.exponentialRampToValueAtTime(150, t + 0.08)
    const og = ctx.createGain()
    env(og, t, 0.002, 0.08 * v, 0, 0.08)
    o.connect(og)
    og.connect(bus)
    o.start(t)
    o.stop(t + 0.15)
  }
  function hat(t, v, open) {
    const s = ctx.createBufferSource()
    s.buffer = noise
    const f = ctx.createBiquadFilter()
    f.type = 'highpass'
    f.frequency.value = 8000
    const g = ctx.createGain()
    env(g, t, 0.002, v, 0, open ? 0.2 : 0.04)
    s.connect(f)
    f.connect(g)
    g.connect(bus)
    s.start(t, Math.random() * 0.2)
    s.stop(t + 0.3)
  }

  function play(i, t) {
    const bar = Math.floor(i / 16) % 4
    const pos = i % 16
    const section = Math.floor(i / 64)
    // intro: drums and bass; then keys; then guitar; then the lick - and loop the last three
    const part = section === 0 ? 0 : 1 + ((section - 1) % 3)
    const keys = part >= 1
    const gtr = part >= 2
    const lead = part === 3

    // drums
    if (pos === 0 || pos === 7 || pos === 10) kick(t, pos === 0 ? 1 : 0.75)
    if (keys && (pos === 4 || pos === 12)) snare(t, 1)
    if (keys && pos === 15 && bar === 3) snare(t, 0.35)
    if (pos === 14) hat(t, 0.03, true)
    else hat(t, HATS[pos % 8] * (part === 0 ? 0.8 : 1), false)

    // bass
    const b = BASS[bar][pos]
    if (b) bass(b[0], t, b[1], b[2])

    // keys
    if (keys && STABS[pos]) {
      const chord = pos >= 8 && KEYS_B[bar] ? KEYS_B[bar] : KEYS[bar]
      stab(chord, t, STABS[pos])
    }
    if (gtr) scratch(t, SCRATCH[pos])
    if (lead && LICK[bar] && LICK[bar][pos]) lick(LICK[bar][pos], t)
  }
  function schedule() {
    while (nextTime < ctx.currentTime + 0.15) {
      const swing = step % 2 === 1 ? S16 * SWING : 0
      play(step, nextTime + swing)
      nextTime += S16
      step++
    }
  }
  return {
    on() {
      if (!ctx) init()
      clearTimeout(offTimer)
      ctx.resume()
      if (!timer) {
        nextTime = ctx.currentTime + 0.06
        timer = setInterval(schedule, 25)
      }
      master.gain.cancelScheduledValues(ctx.currentTime)
      master.gain.setValueAtTime(Math.max(master.gain.value, 0.0001), ctx.currentTime)
      master.gain.linearRampToValueAtTime(0.9, ctx.currentTime + 1.2)
    },
    off() {
      if (!ctx) return
      master.gain.cancelScheduledValues(ctx.currentTime)
      master.gain.setValueAtTime(master.gain.value, ctx.currentTime)
      master.gain.linearRampToValueAtTime(0, ctx.currentTime + 0.4)
      offTimer = setTimeout(() => {
        clearInterval(timer)
        timer = null
        ctx.suspend()
      }, 450)
    },
    hold() {
      if (ctx && timer) ctx.suspend()
    },
    release() {
      if (ctx && timer) ctx.resume()
    },
    // The page is going away: stop the scheduler, let the context go.
    close() {
      clearTimeout(offTimer)
      clearInterval(timer)
      timer = null
      if (ctx) ctx.close()
      ctx = null
    },
  }
}

/* ---------- Scenes ---------- */

// A number counting up to its value when its scene comes on (the player
// mounts a scene again each time it comes on).
function CountUp({ to }) {
  const [n, setN] = useState(() => (reducedMotion() ? to : 0))
  useEffect(() => {
    if (reducedMotion()) return
    const t0 = performance.now()
    let raf
    const f = (now) => {
      const p = Math.min((now - t0) / 1200, 1)
      setN(Math.round(to * (1 - Math.pow(1 - p, 3))))
      if (p < 1) raf = requestAnimationFrame(f)
    }
    raf = requestAnimationFrame(f)
    return () => cancelAnimationFrame(raf)
  }, [to])
  return <span>{n}</span>
}

// A screenshot from the docs (@screens), the theme's one shown (tour.css).
function Screenshot({ light, dark, alt }) {
  return (
    <>
      <img className="img-l" src={light} alt={alt} width="1280" height="800" loading="lazy" decoding="async" />
      <img className="img-d" src={dark} alt={alt} width="1280" height="800" loading="lazy" decoding="async" />
    </>
  )
}

function Hero() {
  return (
    <>
      <span className="sym" aria-hidden="true" dangerouslySetInnerHTML={{ __html: symbol }} />
      <div>
        <p className="eyebrow win" style={{ '--d': '.9s' }}>
          Janus · alpha
        </p>
        <h1 className="win" style={{ '--d': '1.1s' }}>
          An immutable Linux
          <br />
          for your <span className="accent">HAProxy</span> load balancers
        </h1>
        <p className="lead win" style={{ '--d': '1.4s' }}>
          A tiny, API-driven operating system built around HAProxy. It boots a verified, read-only image, has no shell to break into, and updates itself atomically, with a rollback when
          something goes wrong.
        </p>
      </div>
      <div className="facts">
        <div className="win" style={{ '--d': '2.1s' }}>
          <strong>
            <CountUp to={15} /> MB
          </strong>
          <span>root filesystem, 4 binaries</span>
        </div>
        <div className="win" style={{ '--d': '2.4s' }}>
          <strong>0</strong>
          <span>shells, SSH daemons, package managers</span>
        </div>
        <div className="win" style={{ '--d': '2.7s' }}>
          <strong>mTLS</strong>
          <span>on every API call</span>
        </div>
        <div className="win" style={{ '--d': '3.0s' }}>
          <strong>A/B</strong>
          <span>updates, signed, with rollback</span>
        </div>
      </div>
    </>
  )
}

function Access() {
  return (
    <>
      <div className="copy">
        <p className="eyebrow win">Access</p>
        <h2 className="win" style={{ '--d': '.1s' }}>
          Nothing to log into
        </h2>
        <ul className="nope win" style={{ '--d': '.25s' }}>
          <li style={{ '--s': '1.0s' }}>SSH daemon</li>
          <li style={{ '--s': '1.4s' }}>interactive shell</li>
          <li style={{ '--s': '1.8s' }}>package manager</li>
        </ul>
        <p className="lead win" style={{ '--d': '.4s' }}>
          Every action goes through one gRPC API, with mutual TLS on every call. Three roles decide who may do what.
        </p>
        <ul className="chips">
          <li className="chip a win" style={{ '--d': '3.2s' }}>
            admin
          </li>
          <li className="chip a win" style={{ '--d': '3.5s' }}>
            operator
          </li>
          <li className="chip a win" style={{ '--d': '3.8s' }}>
            reader
          </li>
        </ul>
        <p className="note win" style={{ '--d': '4.2s' }}>
          janusctl signs in with your SSH key. Controller accounts add MFA with TOTP or passkeys, API tokens and access by node label.
        </p>
      </div>
      <div className="vis">
        <svg viewBox="0 0 470 320" role="img" aria-label="janusctl and the Janus Controller reach janusd on the node over gRPC with mutual TLS">
          <g className="win" style={{ '--d': '.5s' }}>
            <rect className="box" x="6" y="58" width="134" height="56" rx="10" />
            <text x="73" y="84" textAnchor="middle" className="b" style={{ fontSize: '15px' }}>
              janusctl
            </text>
            <text x="73" y="103" textAnchor="middle" className="m sm">
              terminal
            </text>
          </g>
          <g className="win" style={{ '--d': '.7s' }}>
            <rect className="box" x="6" y="206" width="134" height="56" rx="10" />
            <text x="73" y="232" textAnchor="middle" className="b" style={{ fontSize: '15px' }}>
              Controller
            </text>
            <text x="73" y="251" textAnchor="middle" className="m sm">
              browser
            </text>
          </g>
          <path className="ln" d="M140 86L204 150M140 234L204 170M264 160H322" />
          <g className="pop" style={{ '--d': '1.0s' }}>
            <circle cx="234" cy="160" r="31" fill="var(--surface)" stroke="var(--accent)" strokeWidth="2" />
            <rect x="221" y="158" width="26" height="20" rx="4" fill="var(--accent)" />
            <path d="M225 158v-6a9 9 0 0 1 18 0v6" fill="none" stroke="var(--accent)" strokeWidth="3.2" />
          </g>
          <text className="ai win" style={{ '--d': '1.2s' }} x="234" y="216" textAnchor="middle">
            gRPC · mTLS
          </text>
          <g className="win" style={{ '--d': '.9s' }}>
            <rect x="322" y="88" width="142" height="144" rx="12" fill="var(--surface)" stroke="var(--accent)" strokeWidth="2" />
            <text x="393" y="124" textAnchor="middle" className="b">
              Janus node
            </text>
            <rect x="338" y="140" width="110" height="28" rx="6" fill="var(--accent-soft)" />
            <text x="393" y="159" textAnchor="middle" className="ai sm">
              janusd :9505
            </text>
            <rect x="338" y="176" width="110" height="28" rx="6" fill="var(--surface-2)" />
            <text x="393" y="195" textAnchor="middle" className="sm">
              HAProxy
            </text>
          </g>
          <circle className="pk" fill="var(--accent)" cx="140" cy="86" r="4.5" style={{ '--dx': '64px', '--dy': '64px', '--t': '2.2s', '--d': '1.2s' }} />
          <circle className="pk" fill="var(--accent)" cx="140" cy="234" r="4.5" style={{ '--dx': '64px', '--dy': '-64px', '--t': '2.2s', '--d': '2.3s' }} />
          <circle className="pk" fill="var(--accent)" cx="264" cy="160" r="4.5" style={{ '--dx': '58px', '--dy': '0px', '--t': '1.5s', '--d': '1.7s' }} />
        </svg>
      </div>
    </>
  )
}

// An excerpt of kernel/configs/janus_stable_defconfig, scrolled in a loop
// (twice over, the animation moves it up by half).
const KCONFIG = [
  '# CONFIG_MODULES is not set',
  '# CONFIG_BPF_SYSCALL is not set',
  '# CONFIG_USERFAULTFD is not set',
  '# CONFIG_DEVMEM is not set',
  'CONFIG_DM_VERITY=y',
  'CONFIG_SECURITY_SELINUX=y',
  'CONFIG_RANDOMIZE_BASE=y',
  'CONFIG_STACKPROTECTOR_STRONG=y',
  'CONFIG_INIT_ON_ALLOC_DEFAULT_ON=y',
  '# CONFIG_SOUND is not set',
  '# CONFIG_DRM is not set',
  '# CONFIG_BT is not set',
  'CONFIG_EFI_STUB=y',
  'CONFIG_KEXEC_FILE=y',
]

function Light() {
  return (
    <>
      <div className="copy">
        <p className="eyebrow win">Footprint</p>
        <h2 className="win" style={{ '--d': '.1s' }}>
          Light, down to the byte
        </h2>
        <p className="lead win" style={{ '--d': '.2s' }}>
          Built from scratch, LFS-style, from a kernel configured symbol by symbol. Optional software is chosen per image, never installed afterwards: an image carries exactly what you
          picked.
        </p>
        <p className="note win" style={{ '--d': '.4s' }}>
          It boots straight to a serving HAProxy.
        </p>
      </div>
      <div className="vis">
        <div className="rootfs">
          <div className="rootfs-h win" style={{ '--d': '.3s' }}>
            <span>read-only root filesystem</span>
            <b>
              <CountUp to={15} /> MB
            </b>
          </div>
          <div className="bin win" style={{ '--d': '.8s' }}>
            <code>/sbin/init</code>
            <span>a Go init and its shutdown command</span>
          </div>
          <div className="bin win" style={{ '--d': '1.2s' }}>
            <code>/sbin/janusd</code>
            <span>the API daemon, the control plane</span>
          </div>
          <div className="bin win" style={{ '--d': '1.6s' }}>
            <code>/usr/local/sbin/haproxy</code>
            <span>the data plane, static</span>
          </div>
        </div>
        <div className="row2">
          <div className="tile win" style={{ '--d': '2.2s' }}>
            <b>52 MB VM disk</b>
            <span>compressed, both update slots included</span>
            <div className="diskbar">
              <i className="grow" style={{ '--d': '2.5s', '--len': '1.2s', width: '50%', background: 'var(--accent)' }} />
              <i className="grow" style={{ '--d': '3.2s', '--len': '1.2s', width: '50%', background: 'var(--accent-soft)' }} />
            </div>
            <span className="mono">slot A · slot B</span>
          </div>
          <div className="tile win" style={{ '--d': '2.6s', padding: 0, border: 0, background: 'none' }}>
            <div className="kcfg" aria-label="Excerpt of janus_stable_defconfig">
              <pre>
                {[...KCONFIG, ...KCONFIG].map((line, i) => (
                  <Fragment key={i}>
                    {i > 0 && '\n'}
                    {line.startsWith('#') ? line : <b>{line}</b>}
                  </Fragment>
                ))}
              </pre>
            </div>
          </div>
        </div>
      </div>
    </>
  )
}

function Secure() {
  return (
    <>
      <div className="copy">
        <p className="eyebrow win">Trusted boot</p>
        <h2 className="win" style={{ '--d': '.1s' }}>
          Secure by construction
        </h2>
        <p className="lead win" style={{ '--d': '.2s' }}>
          Trust starts in the firmware and holds all the way up to HAProxy. Each link is checked before the next one runs.
        </p>
      </div>
      <div className="vis">
        <ol className="chain">
          <li className="win" style={{ '--d': '.5s' }}>
            <span className="dot" style={{ '--d': '1.0s' }}>
              <svg viewBox="0 0 24 24">
                <path fill="currentColor" d="M12 2 4 5v6c0 5 3.4 9.5 8 11 4.6-1.5 8-6 8-11V5z" />
              </svg>
            </span>
            <div>
              <b>Secure Boot</b>
              <span>The kernel image is signed and carries the root filesystem&apos;s hash.</span>
            </div>
          </li>
          <li className="win" style={{ '--d': '1.2s' }}>
            <span className="dot" style={{ '--d': '1.8s' }}>
              <svg viewBox="0 0 24 24">
                <path fill="currentColor" d="M3 5h18v4H3zm0 5h18v4H3zm0 5h18v4H3z" />
              </svg>
            </span>
            <div>
              <b>dm-verity</b>
              <span>A read-only root, verified block by block against that hash.</span>
            </div>
          </li>
          <li className="win" style={{ '--d': '2.0s' }}>
            <span className="dot" style={{ '--d': '2.6s' }}>
              <svg viewBox="0 0 24 24">
                <path fill="currentColor" d="M6 10V8a6 6 0 1 1 12 0v2h1v12H5V10zm2 0h8V8a4 4 0 0 0-8 0z" />
              </svg>
            </span>
            <div>
              <b>SELinux enforcing</b>
              <span>A hand-written policy confines janusd, HAProxy and every extension.</span>
            </div>
          </li>
          <li className="win" style={{ '--d': '2.8s' }}>
            <span className="dot" style={{ '--d': '3.4s' }}>
              <svg viewBox="0 0 24 24">
                <path fill="currentColor" d="M4 6h10v2H4zm0 5h16v2H4zm0 5h7v2H4zm12-12h4v6h-4z" />
              </svg>
            </span>
            <div>
              <b>CIS Debian 13 benchmark, Level 2 Server</b>
              <span>Its kernel parameters are written at every boot, then locked.</span>
            </div>
          </li>
          <li className="win" style={{ '--d': '3.6s' }}>
            <span className="dot" style={{ '--d': '4.2s' }}>
              <svg viewBox="0 0 24 24">
                <path fill="currentColor" d="m9.5 16.2-4.2-4.2-1.4 1.4 5.6 5.6 11-11-1.4-1.4z" />
              </svg>
            </span>
            <div>
              <b>Signed updates only</b>
              <span>A new version installs only when signed with a Janus release key.</span>
            </div>
          </li>
        </ol>
      </div>
    </>
  )
}

function HAProxy() {
  return (
    <>
      <div className="copy">
        <p className="eyebrow win">Data plane</p>
        <h2 className="win" style={{ '--d': '.1s' }}>
          HAProxy LTS at the core
        </h2>
        <p className="lead win" style={{ '--d': '.2s' }}>
          The LTS branch you choose, built static with AWS-LC for fast TLS and the Prometheus exporter.
        </p>
        <ul className="chips">
          <li className="chip a win" style={{ '--d': '.5s' }}>
            3.4
          </li>
          <li className="chip a win" style={{ '--d': '.65s' }}>
            3.2
          </li>
          <li className="chip a win" style={{ '--d': '.8s' }}>
            3.0
          </li>
          <li className="chip win" style={{ '--d': '.95s' }}>
            AWS-LC
          </li>
          <li className="chip win" style={{ '--d': '1.1s' }}>
            Prometheus exporter
          </li>
        </ul>
        <ul className="pts">
          <li className="win" style={{ '--d': '1.4s' }}>
            Seamless reloads
          </li>
          <li className="win" style={{ '--d': '1.7s' }}>
            Runtime maps, ACLs and certificates through the API
          </li>
          <li className="win" style={{ '--d': '2.0s' }}>
            Each configuration checked by the node before it applies
          </li>
          <li className="win" style={{ '--d': '2.3s' }}>
            Let&apos;s Encrypt certificates as an extension
          </li>
        </ul>
      </div>
      <div className="vis">
        <svg viewBox="0 0 470 330" role="img" aria-label="Clients reach HAProxy's frontend; its backend spreads requests over three servers">
          <g className="win" style={{ '--d': '.3s' }}>
            <circle className="box" cx="24" cy="70" r="11" />
            <circle className="box" cx="24" cy="160" r="11" />
            <circle className="box" cx="24" cy="250" r="11" />
            <text className="m sm" x="24" y="292" textAnchor="middle">
              clients
            </text>
          </g>
          <path className="ln" d="M35 70L152 122M35 160H152M35 250L152 198M318 122L388 72M318 160H388M318 198L388 248" />
          <g className="win" style={{ '--d': '.5s' }}>
            <path d="M166 70H235V250H166A14 14 0 0 1 152 236V84A14 14 0 0 1 166 70Z" fill="var(--accent-soft)" />
            <path d="M235 70H304A14 14 0 0 1 318 84V236A14 14 0 0 1 304 250H235Z" fill="var(--info-soft)" />
            <rect x="152" y="70" width="166" height="180" rx="14" fill="none" stroke="var(--border)" strokeWidth="1.5" />
            <text className="ai sm" x="193" y="100" textAnchor="middle">
              frontend
            </text>
            <text className="in sm" x="277" y="100" textAnchor="middle">
              backend
            </text>
            <rect x="225" y="134" width="20" height="52" rx="3" fill="var(--logo-bar)" />
            <path d="M178 160L210 136V184Z" fill="var(--text)" />
            <path d="M292 160L260 136V184Z" fill="var(--text)" />
            <text x="235" y="226" textAnchor="middle" className="b" style={{ fontSize: '14px' }}>
              HAProxy
            </text>
          </g>
          <g className="win" style={{ '--d': '.7s' }}>
            <rect className="box" x="388" y="50" width="76" height="44" rx="8" />
            <text x="426" y="77" textAnchor="middle" className="sm">
              app1
            </text>
            <rect className="box" x="388" y="138" width="76" height="44" rx="8" />
            <text x="426" y="165" textAnchor="middle" className="sm">
              app2
            </text>
            <rect className="box" x="388" y="226" width="76" height="44" rx="8" />
            <text x="426" y="253" textAnchor="middle" className="sm">
              app3
            </text>
          </g>
          <circle className="pk" fill="var(--accent)" cx="35" cy="70" r="4.5" style={{ '--dx': '117px', '--dy': '52px', '--t': '2.4s', '--d': '1s' }} />
          <circle className="pk" fill="var(--accent)" cx="35" cy="160" r="4.5" style={{ '--dx': '117px', '--dy': '0px', '--t': '2.4s', '--d': '1.8s' }} />
          <circle className="pk" fill="var(--accent)" cx="35" cy="250" r="4.5" style={{ '--dx': '117px', '--dy': '-52px', '--t': '2.4s', '--d': '2.6s' }} />
          <circle className="pk" fill="var(--info)" cx="318" cy="122" r="4.5" style={{ '--dx': '70px', '--dy': '-50px', '--t': '2.4s', '--d': '1.6s' }} />
          <circle className="pk" fill="var(--info)" cx="318" cy="160" r="4.5" style={{ '--dx': '70px', '--dy': '0px', '--t': '2.4s', '--d': '2.4s' }} />
          <circle className="pk" fill="var(--info)" cx="318" cy="198" r="4.5" style={{ '--dx': '70px', '--dy': '50px', '--t': '2.4s', '--d': '3.2s' }} />
          <g className="win" style={{ '--d': '1.2s' }}>
            <rect className="box" x="152" y="282" width="166" height="38" rx="8" />
            <text className="m sm" x="166" y="305">
              :10056/metrics
            </text>
          </g>
        </svg>
      </div>
    </>
  )
}

function Updates() {
  return (
    <>
      <div className="copy">
        <p className="eyebrow win">Updates</p>
        <h2 className="win" style={{ '--d': '.1s' }}>
          Atomic A/B updates, with rollback
        </h2>
        <p className="lead win" style={{ '--d': '.2s' }}>
          The new version is written to the idle slot and booted on trial, through kexec without the firmware if you ask. The Controller finds the update built for the node&apos;s own
          schematic, extensions included.
        </p>
      </div>
      <div className="vis">
        <div className="slots">
          <div className="slot slotA win" style={{ '--d': '.3s' }}>
            <h4>
              Slot A{' '}
              <span className="stack">
                <span className="state s-act win" style={{ '--v': '6.9s' }}>
                  active
                </span>
                <span className="state s-idle win later" style={{ '--d': '6.9s' }}>
                  fallback
                </span>
              </span>
            </h4>
            <span className="ver">v2026.10.06</span>
          </div>
          <div className="slot slotB win" style={{ '--d': '.5s' }}>
            <h4>
              Slot B{' '}
              <span className="stack">
                <span className="state s-idle win" style={{ '--v': '1.6s' }}>
                  idle
                </span>
                <span className="state s-busy win later" style={{ '--d': '1.6s', '--v': '4.0s' }}>
                  writing
                </span>
                <span className="state s-acc win later" style={{ '--d': '4.0s', '--v': '6.9s' }}>
                  on trial
                </span>
                <span className="state s-act win later" style={{ '--d': '6.9s' }}>
                  active
                </span>
              </span>
            </h4>
            <span className="ver">
              <span className="stack">
                <span className="win" style={{ '--v': '1.6s' }}>
                  empty
                </span>
                <span className="win later" style={{ '--d': '1.6s' }}>
                  next release
                </span>
              </span>
            </span>
            <div className="track">
              <i className="grow" style={{ '--d': '1.6s', '--len': '2.4s' }} />
            </div>
          </div>
        </div>
        <ol className="steps">
          <li style={{ '--d': '1.0s' }}>
            <i>1</i>Bundle verified against the Janus release key
          </li>
          <li style={{ '--d': '1.6s' }}>
            <i>2</i>Written to the idle slot
          </li>
          <li style={{ '--d': '4.0s' }}>
            <i>3</i>Booted on trial, optionally through kexec
          </li>
          <li style={{ '--d': '5.4s' }}>
            <i>4</i>HAProxy comes back healthy
          </li>
          <li style={{ '--d': '6.9s' }}>
            <i>5</i>Slot B kept; slot A stays as the fallback
          </li>
        </ol>
        <p className="fallback win" style={{ '--d': '7.6s' }}>
          <b>Not healthy?</b> The node switches back to slot A by itself.
        </p>
      </div>
    </>
  )
}

function Trial() {
  return (
    <>
      <div className="copy">
        <p className="eyebrow win">Network &amp; kernel</p>
        <h2 className="win" style={{ '--d': '.1s' }}>
          Every risky change runs on trial
        </h2>
        <p className="lead win" style={{ '--d': '.2s' }}>
          Network, firewall and kernel changes are applied on trial. One that cuts the node off reverts by itself.
        </p>
        <ul className="chips">
          <li className="chip win" style={{ '--d': '.5s' }}>
            static addressing
          </li>
          <li className="chip win" style={{ '--d': '.6s' }}>
            802.1Q VLANs
          </li>
          <li className="chip win" style={{ '--d': '.7s' }}>
            several interfaces
          </li>
          <li className="chip win" style={{ '--d': '.8s' }}>
            DNS
          </li>
          <li className="chip win" style={{ '--d': '.9s' }}>
            NTP
          </li>
        </ul>
        <p className="note win" style={{ '--d': '1.2s' }}>
          Kernel tuning covers the parameters HAProxy depends on, with values suggested from what the node observes. The CIS benchmark&apos;s stay locked.
        </p>
      </div>
      <div className="vis">
        <svg viewBox="0 0 470 320" role="img" aria-label="A change is applied, runs on a countdown, then is kept if the node stays reachable or reverted if it is cut off">
          <g className="win" style={{ '--d': '.4s' }}>
            <rect className="box" x="6" y="112" width="150" height="96" rx="12" />
            <text x="22" y="140" className="m sm">
              apply
            </text>
            <text x="22" y="164" className="b" style={{ fontSize: '15px' }}>
              VLAN 120
            </text>
            <text x="22" y="186" className="sm">
              on eth0
            </text>
          </g>
          <path className="ln" d="M156 160H186" />
          <g className="win" style={{ '--d': '1.0s' }}>
            <circle className="ring-bg" cx="236" cy="160" r="45" />
            <circle className="ring-fg" cx="236" cy="160" r="45" />
            <text x="236" y="156" textAnchor="middle" className="ai sm">
              on
            </text>
            <text x="236" y="174" textAnchor="middle" className="ai sm">
              trial
            </text>
          </g>
          <path className="ln" d="M286 140C320 140 318 76 352 76M286 180C320 180 318 244 352 244" />
          <g className="win" style={{ '--d': '4.6s' }}>
            <rect className="outc ok" x="352" y="36" width="114" height="80" rx="12" />
            <text x="368" y="64" className="okc sm" style={{ fontWeight: 600 }}>
              reachable
            </text>
            <text x="368" y="86" className="sm">
              confirmed,
            </text>
            <text x="368" y="104" className="sm">
              kept
            </text>
          </g>
          <g className="win" style={{ '--d': '5.2s' }}>
            <rect className="outc" x="352" y="204" width="114" height="80" rx="12" strokeDasharray="5 5" />
            <text x="368" y="232" className="sm" style={{ fill: 'var(--danger)', fontWeight: 600 }}>
              cut off
            </text>
            <text x="368" y="254" className="m sm">
              reverted
            </text>
            <text x="368" y="272" className="m sm">
              by itself
            </text>
          </g>
        </svg>
      </div>
    </>
  )
}

function Extensions() {
  return (
    <>
      <div className="copy">
        <p className="eyebrow win">Image builder</p>
        <h2 className="win" style={{ '--d': '.1s' }}>
          Pick your extensions, get your image
        </h2>
        <p className="lead win" style={{ '--d': '.2s' }}>
          Choose a platform and extensions in the image builder. They become part of the signed image, and the same choices always give the same schematic ID.
        </p>
        <p className="note win" style={{ '--d': '.5s' }}>
          keepalived and BIRD follow HAProxy&apos;s health: if it stops answering, BGP withdraws the node&apos;s routes and VRRP hands the virtual IP to the other node.
        </p>
      </div>
      <div className="vis">
        <svg viewBox="0 0 470 340" role="img" aria-label="Seven extensions drop into one signed image, which gets a schematic ID">
          <rect className="imgbox win" style={{ '--d': '.3s' }} x="40" y="22" width="390" height="250" rx="16" />
          <g className="win" style={{ '--d': '.4s' }}>
            <text x="60" y="52" className="b">
              Your Janus image
            </text>
            <text x="410" y="52" textAnchor="end" className="m sm">
              signed
            </text>
          </g>
          <g className="win" style={{ '--d': '.5s' }}>
            <rect x="60" y="68" width="350" height="34" rx="8" fill="var(--accent-soft)" />
            <text x="76" y="90" className="ai sm">
              base · kernel · janusd · HAProxy 3.4
            </text>
          </g>
          <g className="ext" style={{ '--d': '1.0s', '--fx': '-60px' }}>
            <rect x="60" y="116" width="170" height="38" rx="8" />
            <text x="74" y="135" className="sm" style={{ fontWeight: 600 }}>
              Firewall
            </text>
            <text x="74" y="149" className="m" style={{ fontSize: '10px' }}>
              nftables
            </text>
          </g>
          <g className="ext" style={{ '--d': '1.4s', '--fx': '60px' }}>
            <rect x="240" y="116" width="170" height="38" rx="8" />
            <text x="254" y="135" className="sm" style={{ fontWeight: 600 }}>
              VRRP
            </text>
            <text x="254" y="149" className="m" style={{ fontSize: '10px' }}>
              keepalived
            </text>
          </g>
          <g className="ext" style={{ '--d': '1.8s', '--fx': '-60px' }}>
            <rect x="60" y="162" width="170" height="38" rx="8" />
            <text x="74" y="181" className="sm" style={{ fontWeight: 600 }}>
              BGP
            </text>
            <text x="74" y="195" className="m" style={{ fontSize: '10px' }}>
              BIRD
            </text>
          </g>
          <g className="ext" style={{ '--d': '2.2s', '--fx': '60px' }}>
            <rect x="240" y="162" width="170" height="38" rx="8" />
            <text x="254" y="181" className="sm" style={{ fontWeight: 600 }}>
              Let&apos;s Encrypt
            </text>
            <text x="254" y="195" className="m" style={{ fontSize: '10px' }}>
              certificates
            </text>
          </g>
          <g className="ext" style={{ '--d': '2.6s', '--fx': '-60px' }}>
            <rect x="60" y="208" width="112" height="38" rx="8" />
            <text x="74" y="231" className="sm" style={{ fontWeight: 600 }}>
              Consul
            </text>
          </g>
          <g className="ext" style={{ '--d': '3.0s' }}>
            <rect x="179" y="208" width="112" height="38" rx="8" />
            <text x="191" y="231" className="sm" style={{ fontWeight: 600, fontSize: '11px' }}>
              node_exporter
            </text>
          </g>
          <g className="ext" style={{ '--d': '3.4s', '--fx': '60px' }}>
            <rect x="298" y="208" width="112" height="38" rx="8" />
            <text x="310" y="225" className="sm" style={{ fontWeight: 600, fontSize: '11px' }}>
              QEMU guest
            </text>
            <text x="310" y="239" className="m" style={{ fontSize: '10px' }}>
              agent
            </text>
          </g>
          <g className="pop" style={{ '--d': '4.2s' }}>
            <rect x="120" y="290" width="230" height="40" rx="20" fill="var(--accent)" />
            <text x="235" y="315" textAnchor="middle" style={{ fill: 'var(--accent-text)', fontWeight: 600 }}>
              schematic 74593292
            </text>
          </g>
        </svg>
      </div>
    </>
  )
}

function Controller() {
  return (
    <>
      <div className="copy">
        <p className="eyebrow win">Janus Controller</p>
        <h2 className="win" style={{ '--d': '.1s' }}>
          Run a fleet from one page
        </h2>
        <ul className="pts">
          <li className="win" style={{ '--d': '.4s' }}>
            Nodes register with your Controller on first boot
          </li>
          <li className="win" style={{ '--d': '.7s' }}>
            Creates, resizes and updates VMs on libvirt and Proxmox VE
          </li>
          <li className="win" style={{ '--d': '1.0s' }}>
            Roles, MFA, API tokens and an audit log
          </li>
          <li className="win" style={{ '--d': '1.3s' }}>
            A Terraform provider drives it as code
          </li>
        </ul>
        <p className="note win" style={{ '--d': '1.6s' }}>
          Ships as a Docker image: <code>swenske/janus-controller</code>
        </p>
      </div>
      <div className="vis">
        <div className="shots">
          <figure className="shot win" style={{ '--d': '.2s', '--v': '4.4s', '--o': '30% 20%', '--z': '1.1', '--kb': '4.6s' }}>
            <Screenshot light={nodesLight} dark={nodesDark} alt="The Controller's node list: three nodes online, with labels, version, HAProxy health, uptime and fleet trust" />
            <figcaption>
              <b>Your fleet at a glance:</b> every node, its release, its HAProxy, its trust.
            </figcaption>
          </figure>
          <figure className="shot win later" style={{ '--d': '4.3s', '--v': '8.4s', '--o': '60% 30%', '--z': '1.14', '--kb': '4.4s' }}>
            <Screenshot light={overviewLight} dark={overviewDark} alt="A node's overview: CPU, memory, HAProxy connections and uptime, with live charts" />
            <figcaption>
              <b>Each node live:</b> its system and its traffic.
            </figcaption>
          </figure>
          <figure className="shot win later" style={{ '--d': '8.3s', '--o': '40% 40%', '--z': '1.12', '--kb': '4.4s' }}>
            <Screenshot light={configLight} dark={configDark} alt="A node's HAProxy configuration in the Controller's editor" />
            <figcaption>
              <b>HAProxy configured from the page,</b> checked by the node before it applies.
            </figcaption>
          </figure>
        </div>
        <div className="shot-dots" aria-hidden="true">
          <i style={{ '--d': '.2s', '--v': '4.3s' }} />
          <i style={{ '--d': '4.3s', '--v': '8.3s' }} />
          <i style={{ '--d': '8.3s' }} />
        </div>
      </div>
    </>
  )
}

function Audit() {
  return (
    <>
      <div className="copy">
        <p className="eyebrow win">Audit</p>
        <h2 className="win" style={{ '--d': '.1s' }}>
          Every change, on the record
        </h2>
        <p className="lead win" style={{ '--d': '.2s' }}>
          The Controller&apos;s Audit tab lists every change made from its pages or with an API token, and every sign-in, failed or not, with who made it.
        </p>
        <ul className="pts">
          <li className="win" style={{ '--d': '.6s' }}>
            What the Controller does on a node, the node logs too, under the same name
          </li>
          <li className="win" style={{ '--d': '.9s' }}>
            janusctl&apos;s short certificates carry the account and its role: each node logs who called it
          </li>
          <li className="win" style={{ '--d': '1.2s' }}>
            Kept in <code>audit.jsonl</code> and the container&apos;s log, and part of every backup
          </li>
          <li className="win" style={{ '--d': '1.5s' }}>
            Admins filter the newest entries by account
          </li>
        </ul>
      </div>
      <div className="vis">
        <div className="alog win" style={{ '--d': '.3s' }}>
          <div className="alog-h">
            Audit <span>example entries</span>
          </div>
          <div className="arow win" style={{ '--d': '.8s' }}>
            <time>09:01:52</time>
            <span className="who">bob</span>
            <span className="what">sign-in</span>
            <Badge tone="danger">failed</Badge>
          </div>
          <div className="arow win" style={{ '--d': '1.3s' }}>
            <time>09:02:05</time>
            <span className="who">alice</span>
            <span className="what">
              sign-in <small>· passkey</small>
            </span>
            <Badge tone="ok">ok</Badge>
          </div>
          <div className="arow win hl" style={{ '--d': '1.8s', '--h': '4.4s' }}>
            <time>09:02:14</time>
            <span className="who">alice</span>
            <span className="what">
              HAProxy config applied <small>· edge-par-1</small>
            </span>
            <Badge tone="ok">ok</Badge>
          </div>
          <div className="arow win hl" style={{ '--d': '2.3s', '--h': '5.4s' }}>
            <time>09:14:52</time>
            <span className="who">alice</span>
            <span className="what">
              server app2 → drain <small>· edge-par-1</small>
            </span>
            <Badge tone="ok">ok</Badge>
          </div>
          <div className="arow win hl" style={{ '--d': '2.8s', '--h': '6.4s' }}>
            <time>09:27:40</time>
            <span className="who">alice</span>
            <span className="what">
              packet capture <small>· edge-par-1</small>
            </span>
            <Badge tone="ok">ok</Badge>
          </div>
          <div className="arow win" style={{ '--d': '3.3s' }}>
            <time>09:41:03</time>
            <span className="who">terraform</span>
            <span className="what">
              node labels changed <small>· edge-par-3</small>
            </span>
            <Badge tone="info">API token</Badge>
          </div>
        </div>
        <div className="nlog win" style={{ '--d': '3.9s' }}>
          <div className="t">edge-par-1 · janusd log</div>
          <div className="win" style={{ '--d': '4.4s' }}>
            api: HAProxyService/ApplyConfig: <b>alice (os:operator)</b> via janus-controller
          </div>
          <div className="win" style={{ '--d': '5.4s' }}>
            api: HAProxyService/ServerSetState: <b>alice (os:operator)</b>
          </div>
          <div className="win" style={{ '--d': '6.4s' }}>
            api: SystemService/PacketCapture: <b>alice (os:operator)</b>
          </div>
        </div>
      </div>
    </>
  )
}

function Backups() {
  return (
    <>
      <div className="copy">
        <p className="eyebrow win">Backups</p>
        <h2 className="win" style={{ '--d': '.1s' }}>
          The Controller, backed up to any S3 bucket
        </h2>
        <p className="lead win" style={{ '--d': '.2s' }}>
          Accounts, nodes, the fleet, the audit and the master key, plus each node&apos;s configuration read through its API, never its private keys. Everything is encrypted with age to
          your backup kit. The Controller keeps only the public half: it writes backups it can&apos;t read.
        </p>
        <ul className="pts">
          <li className="win" style={{ '--d': '.6s' }}>
            Once a day by default, the last 30 kept; a failed backup is retried an hour later
          </li>
          <li className="win" style={{ '--d': '.9s' }}>
            A manifest signed with Ed25519: a changed byte is refused
          </li>
          <li className="win" style={{ '--d': '1.2s' }}>
            Write-only credentials and Object Lock: whoever took the Controller couldn&apos;t erase past backups
          </li>
          <li className="win" style={{ '--d': '1.5s' }}>
            Restore from a new Controller&apos;s first page; the nodes keep trusting it
          </li>
        </ul>
      </div>
      <div className="vis">
        <svg
          viewBox="0 0 470 350"
          role="img"
          aria-label="The Controller's data and its nodes' configurations become one encrypted, signed .janusbackup object in an S3 bucket; a new Controller restores it with the backup kit"
        >
          <g className="win" style={{ '--d': '.3s' }}>
            <rect className="box" x="6" y="14" width="146" height="128" rx="12" />
            <text x="20" y="40" className="b" style={{ fontSize: '15px' }}>
              Controller
            </text>
            <text x="20" y="64" className="m sm">
              accounts · MFA
            </text>
            <text x="20" y="84" className="m sm">
              nodes · fleet
            </text>
            <text x="20" y="104" className="m sm">
              audit
            </text>
            <text x="20" y="124" className="m sm">
              master key
            </text>
          </g>
          <g className="win" style={{ '--d': '.6s' }}>
            <rect className="box" x="6" y="154" width="146" height="100" rx="12" />
            <text x="20" y="178" className="b" style={{ fontSize: '14px' }}>
              node configs
            </text>
            <text x="20" y="200" className="m sm">
              HAProxy · network
            </text>
            <text x="20" y="220" className="m sm">
              firewall · VRRP
            </text>
            <text x="20" y="240" className="m sm">
              BGP · Consul
            </text>
          </g>
          <path className="ln" d="M152 78C170 78 168 120 184 120M152 195C170 195 168 150 184 150M304 135H330" />
          <g className="win" style={{ '--d': '1.2s' }}>
            <rect x="184" y="70" width="120" height="132" rx="12" fill="var(--surface)" stroke="var(--accent)" strokeWidth="2" />
            <text x="244" y="96" textAnchor="middle" className="ai sm" style={{ fontWeight: 600 }}>
              .janusbackup
            </text>
            <rect x="198" y="108" width="92" height="26" rx="6" fill="var(--accent-soft)" />
            <text x="244" y="125" textAnchor="middle" className="ai sm">
              age · tar.gz
            </text>
            <rect x="198" y="140" width="92" height="26" rx="6" fill="var(--surface-2)" />
            <text x="244" y="157" textAnchor="middle" className="sm">
              Ed25519 ✓
            </text>
            <text x="244" y="188" textAnchor="middle" className="m" style={{ fontSize: '11px' }}>
              tens of KiB
            </text>
          </g>
          <g className="pop" style={{ '--d': '1.9s' }}>
            <circle cx="300" cy="74" r="15" fill="var(--accent)" />
            <rect x="293" y="73" width="14" height="10" rx="2" fill="var(--accent-text)" />
            <path d="M295.5 73v-3a4.5 4.5 0 0 1 9 0v3" fill="none" stroke="var(--accent-text)" strokeWidth="2" />
          </g>
          <g className="win" style={{ '--d': '2.2s' }}>
            <path d="M330 66v116a67 13 0 0 0 134 0V66" fill="var(--info-soft)" stroke="var(--info)" strokeWidth="2" />
            <ellipse cx="397" cy="66" rx="67" ry="13" fill="var(--surface)" stroke="var(--info)" strokeWidth="2" />
            <text x="397" y="112" textAnchor="middle" className="b" style={{ fontSize: '15px' }}>
              S3 bucket
            </text>
            <text x="397" y="134" textAnchor="middle" className="sm">
              daily · last 30
            </text>
            <text x="397" y="154" textAnchor="middle" className="m" style={{ fontSize: '11px' }}>
              Object Lock
            </text>
          </g>
          <text className="m win" style={{ '--d': '2.6s', fontSize: '11px' }} x="390" y="218" textAnchor="middle">
            AWS · MinIO · Garage
          </text>
          <text className="m win" style={{ '--d': '2.8s', fontSize: '11px' }} x="390" y="234" textAnchor="middle">
            Ceph · Backblaze B2 · R2
          </text>
          <circle className="pk" fill="var(--accent)" cx="152" cy="78" r="4.5" style={{ '--dx': '32px', '--dy': '42px', '--t': '1.6s', '--d': '1.4s' }} />
          <circle className="pk" fill="var(--accent)" cx="152" cy="195" r="4.5" style={{ '--dx': '32px', '--dy': '-45px', '--t': '1.6s', '--d': '2s' }} />
          <circle className="pk" fill="var(--info)" cx="304" cy="135" r="4.5" style={{ '--dx': '26px', '--dy': '0px', '--t': '1.2s', '--d': '2.6s' }} />
          <path className="ln win" style={{ '--d': '4.2s', strokeDasharray: '5 6' }} d="M397 246V290H300" />
          <g className="win" style={{ '--d': '4.6s' }}>
            <rect x="96" y="268" width="204" height="76" rx="12" fill="var(--ok-soft)" stroke="var(--ok)" strokeWidth="2" />
            <text x="112" y="292" className="b" style={{ fontSize: '14px' }}>
              new Controller
            </text>
            <text x="112" y="313" className="sm">
              + backup kit, passphrase
            </text>
            <text x="112" y="333" className="okc sm">
              same fleet, same trust
            </text>
          </g>
          <text className="ai sm win" style={{ '--d': '4.4s' }} x="318" y="282">
            restore
          </text>
        </svg>
      </div>
    </>
  )
}

function Terminal() {
  return (
    <>
      <div className="copy">
        <p className="eyebrow win">janusctl</p>
        <h2 className="win" style={{ '--d': '.1s' }}>
          Or from your terminal
        </h2>
        <p className="lead win" style={{ '--d': '.2s' }}>
          janusctl tui shows a node live, or the whole fleet with one line per node.
        </p>
        <ul className="pts">
          <li className="win" style={{ '--d': '.5s' }}>
            No shell, still observable: logs, kernel messages, processes, files and packet capture over the API, read-only and scoped
          </li>
          <li className="win" style={{ '--d': '.8s' }}>
            Drain a server, reload HAProxy, follow events
          </li>
          <li className="win" style={{ '--d': '1.1s' }}>
            Works through the Controller, or keeps a fleet on its own
          </li>
        </ul>
        <p className="note win mono" style={{ '--d': '1.4s' }}>
          apt install janusctl · amd64, arm64 · bash, zsh, fish
        </p>
      </div>
      <div className="vis">
        <div className="termbar win" style={{ '--d': '.2s' }}>
          <span className="p">$</span>
          <span className="typed">janusctl tui -n edge-par-1</span>
        </div>
        <div className="shots">
          <figure className="shot win" style={{ '--d': '1.8s', '--o': '72% 25%', '--z': '1.35', '--kb': '7s' }}>
            <Screenshot
              light={tuiLight}
              dark={tuiDark}
              alt="janusctl tui on a node: graphs of CPU, memory, network and HAProxy sessions, frontends and backends, services, processes, VRRP and firewall, events and logs"
            />
          </figure>
        </div>
      </div>
    </>
  )
}

function Workflow() {
  return (
    <>
      <div>
        <p className="eyebrow win">Workflow</p>
        <h2 className="win" style={{ '--d': '.1s' }}>
          From image to fleet
        </h2>
      </div>
      <div style={{ width: '100%', maxWidth: '980px', textAlign: 'left' }}>
        <ol className="pipe">
          <li className="win" style={{ '--d': '.5s' }}>
            <span className="n" style={{ '--d': '.9s' }}>
              1
            </span>
            <b>Build</b>
            <span>Choose your platform and extensions in the image builder. Your choices get a schematic ID.</span>
          </li>
          <li className="win" style={{ '--d': '1.4s' }}>
            <span className="n" style={{ '--d': '1.8s' }}>
              2
            </span>
            <b>Boot</b>
            <span>
              Import the disk in Proxmox, KVM or VMware, flash the ISO or the Raspberry Pi image, or let the Controller create the VM. A node prints its admin credentials once, on its
              console.
            </span>
          </li>
          <li className="win" style={{ '--d': '2.3s' }}>
            <span className="n" style={{ '--d': '2.7s' }}>
              3
            </span>
            <b>Manage</b>
            <span>Add it to the Janus Controller or drive it with janusctl: HAProxy configuration, certificates, network, firewall, monitoring.</span>
          </li>
          <li className="win" style={{ '--d': '3.2s' }}>
            <span className="n" style={{ '--d': '3.6s' }}>
              4
            </span>
            <b>Update</b>
            <span>The Controller finds the update built for the node&apos;s schematic and installs it on the idle slot.</span>
          </li>
        </ol>
        <div className="extras">
          <div className="tile win" style={{ '--d': '4.4s' }}>
            <b>qcow2 · VMDK · ISO</b>
            <span>Built and boot-tested under QEMU on every image workflow run</span>
          </div>
          <div className="tile win" style={{ '--d': '4.7s' }}>
            <b>PXE / HTTP Boot</b>
            <span>The same image loaded straight onto real hardware</span>
          </div>
          <div className="tile win" style={{ '--d': '5.0s' }}>
            <b>Provisioning</b>
            <span>Settings at install, offline on an image, or from a NoCloud volume</span>
          </div>
        </div>
      </div>
    </>
  )
}

function Outro() {
  return (
    <>
      <div className="outro-logo win" style={{ '--d': '.2s' }}>
        <img className="wordmark" src={wordmark} alt="Janus" width="296" height="80" />
      </div>
      <p className="lead win" style={{ '--d': '.6s' }}>
        An immutable Linux for your HAProxy load balancers. Open source, MIT licensed.
      </p>
      <div className="ctas win" style={{ '--d': '1.0s' }}>
        <Link to="/builder" className="button primary big">
          Build your image
        </Link>
        <a className="button big" href={`${DOCS}/`}>
          Documentation
        </a>
        <a className="button big" href={REPO}>
          Source on GitHub
        </a>
      </div>
      <p className="warnbox win" style={{ '--d': '1.4s' }}>
        Janus is alpha software: expect breaking changes.
      </p>
      <p className="install win" style={{ '--d': '1.7s' }}>
        janus.sw-servers.net
      </p>
    </>
  )
}

// The scenes in order: the counter's title, how long each plays (ms),
// its layout (hero: one centred column; wide: a larger visual).
const SCENES = [
  { title: 'Janus', dur: 9000, kind: 'hero', Body: Hero },
  { title: 'Nothing to log into', dur: 9000, Body: Access },
  { title: 'Light', dur: 9500, Body: Light },
  { title: 'Secure by construction', dur: 10000, Body: Secure },
  { title: 'HAProxy LTS', dur: 9500, Body: HAProxy },
  { title: 'A/B updates', dur: 11000, Body: Updates },
  { title: 'Changes on trial', dur: 10000, Body: Trial },
  { title: 'Extensions', dur: 10000, Body: Extensions },
  { title: 'Janus Controller', dur: 12500, kind: 'wide', Body: Controller },
  { title: 'Audit', dur: 11000, Body: Audit },
  { title: 'Backups', dur: 12000, Body: Backups },
  { title: 'janusctl tui', dur: 11000, kind: 'wide', Body: Terminal },
  { title: 'From image to fleet', dur: 10000, kind: 'hero', Body: Workflow },
  { title: 'Get Janus', dur: 10000, kind: 'hero', Body: Outro },
]
const TOTAL = SCENES.reduce((sum, sc) => sum + sc.dur, 0)

/* ---------- Player ---------- */

const PAUSE = 'M6 5h4v14H6zM14 5h4v14h-4z'
const PLAY = 'M7 4.5v15l12-7.5z'
const pad2 = (x) => (x < 10 ? '0' : '') + x
const clock = (ms) => {
  const s = Math.floor(ms / 1000)
  return `${Math.floor(s / 60)}:${pad2(s % 60)}`
}

export default function Tour() {
  const n = SCENES.length
  const [idx, setIdx] = useState(0)
  // Bumped on each scene change: the scene that comes on is mounted
  // again, so its animations start over - even when it was already on.
  const [run, setRun] = useState(0)
  const [playing, setPlaying] = useState(() => !reducedMotion())
  const [music, setMusic] = useState('off') // 'on', 'off', or 'none': no Web Audio here
  // What the frame loop and the keyboard read between renders.
  const idxRef = useRef(0)
  const playingRef = useRef(playing)
  const musicRef = useRef('off')
  const elapsed = useRef(0) // ms into the current scene
  const audio = useRef(null)
  const fills = useRef([])
  const timeEl = useRef(null)

  // The progress bar and the clock, written every frame - not renders.
  const paint = useCallback(() => {
    let before = 0
    SCENES.forEach((sc, k) => {
      const v = k < idxRef.current ? 1 : k === idxRef.current ? Math.min(elapsed.current / sc.dur, 1) : 0
      fills.current[k].style.transform = `scaleX(${v})`
      if (k < idxRef.current) before += sc.dur
    })
    timeEl.current.textContent = `${clock(before + elapsed.current)} / ${clock(TOTAL)}`
  }, [])

  const go = useCallback(
    (i) => {
      idxRef.current = (i + n) % n
      elapsed.current = 0
      setIdx(idxRef.current)
      setRun((r) => r + 1)
      paint()
    },
    [n, paint],
  )

  const play = useCallback((p) => {
    playingRef.current = p
    setPlaying(p)
    if (musicRef.current !== 'on') return
    if (p) audio.current.release()
    else audio.current.hold()
  }, [])

  // Started from the click or the key itself: browsers let a page make
  // sound only from a user's gesture.
  const toggleMusic = useCallback(() => {
    const on = musicRef.current !== 'on'
    let state = on ? 'on' : 'off'
    try {
      audio.current ??= createMusic()
      if (on) audio.current.on()
      else audio.current.off()
    } catch {
      state = 'none'
    }
    musicRef.current = state
    setMusic(state)
    if (on && !playingRef.current) play(true)
  }, [play])

  useLayoutEffect(paint, [paint])

  useEffect(() => {
    let raf
    let last = performance.now()
    const tick = (now) => {
      const dt = Math.max(0, Math.min(now - last, 100))
      last = now
      if (playingRef.current && !document.hidden) {
        elapsed.current += dt
        if (elapsed.current >= SCENES[idxRef.current].dur) go(idxRef.current + 1)
        else paint()
      }
      raf = requestAnimationFrame(tick)
    }
    raf = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(raf)
  }, [go, paint])

  useEffect(() => {
    const onKey = (e) => {
      if (e.altKey || e.ctrlKey || e.metaKey) return // the browser's own: Alt+Left is Back
      if (e.target.closest?.('a, input, textarea, select')) return
      if (e.key === 'ArrowRight') go(idxRef.current + 1)
      else if (e.key === 'ArrowLeft') go(idxRef.current - 1)
      else if (e.key === 'm' || e.key === 'M') toggleMusic()
      else if (e.key === ' ' && e.target === document.body) {
        e.preventDefault()
        play(!playingRef.current)
      }
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [go, play, toggleMusic])

  useEffect(() => () => audio.current?.close(), [])

  return (
    <div className="tour">
      <header className="top">
        <div className="top-l">
          <img className="wordmark" src={wordmark} alt="Janus" width="110" height="30" />
          <span className="tag">Feature tour</span>
        </div>
        <div className="count">
          {pad2(idx + 1)} / {pad2(n)} · {SCENES[idx].title}
        </div>
      </header>

      <section className={playing ? 'stage' : 'stage paused'} aria-label="Janus feature tour, animated">
        {SCENES.map(({ title, kind, Body }, k) => (
          <article key={k === idx ? `on-${run}` : title} className={['scene', kind, k === idx && 'on'].filter(Boolean).join(' ')}>
            <Body />
          </article>
        ))}
      </section>

      <nav className="controls" aria-label="Playback">
        <button className="btn" type="button" aria-label="Previous scene" onClick={() => go(idxRef.current - 1)}>
          <svg viewBox="0 0 24 24">
            <path d="M6 5h2.5v14H6zM20 5v14L9.5 12z" />
          </svg>
        </button>
        <button className="btn" type="button" aria-label={playing ? 'Pause' : 'Play'} onClick={() => play(!playingRef.current)}>
          <svg viewBox="0 0 24 24">
            <path d={playing ? PAUSE : PLAY} />
          </svg>
        </button>
        <button className="btn" type="button" aria-label="Next scene" onClick={() => go(idxRef.current + 1)}>
          <svg viewBox="0 0 24 24">
            <path d="M15.5 5H18v14h-2.5zM4 5v14l10.5-7z" />
          </svg>
        </button>
        <button className="btn music" type="button" aria-pressed={music === 'on'} onClick={toggleMusic}>
          <span className="eq" aria-hidden="true">
            <i />
            <i />
            <i />
          </span>
          <span className="lbl">{music === 'on' ? 'Music on' : music === 'none' ? 'No audio' : 'Music off'}</span>
        </button>
        <div className="segs">
          {SCENES.map(({ title, dur }, k) => (
            <button
              key={title}
              type="button"
              className="seg"
              style={{ flexGrow: dur }}
              aria-label={`Scene ${k + 1}: ${title}`}
              title={title}
              aria-current={k === idx ? 'step' : undefined}
              onClick={() => go(k)}
            >
              <i
                ref={(el) => {
                  fills.current[k] = el
                }}
              />
            </button>
          ))}
        </div>
        <span className="time" ref={timeEl} />
      </nav>
    </div>
  )
}
