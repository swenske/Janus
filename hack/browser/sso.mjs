// Single sign-on through a real Keycloak, in the browser (hack/
// controller-sso-test.sh): the Controller at $CONTROLLER, Keycloak at
// $KEYCLOAK (the realm in hack/keycloak/realm-janus.json), both by their
// own names on the test's network. What a person does - the sign-in
// page's button, Keycloak's own form, the page coming back - and what
// the Controller then says (/api/auth/status, /api/users). Screenshots in
// $OUT; no page error, no CSP report, axe (WCAG 2 A/AA) on the pages.
import AxeBuilder from '@axe-core/playwright'
import { createHmac } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { chromium } from 'playwright'

const { CONTROLLER, KEYCLOAK, KC_ADMIN_PASSWORD, OUT, CA } = process.env
const ROOT_PASSWORD = 'sso-test-root-password'
const OLGA_PASSWORD = 'sso-test-olga-password'
const PASSWORDS = { alice: 'alice-password', bob: 'bob-password', carol: 'carol-password', root: 'root-password', 'olga-kc': 'olga-kc-password' }
const problems = []
const browser = await chromium.launch()

const step = (s) => console.log(`sso: ${s}`)
function check(ok, what) {
  if (!ok) throw new Error(what)
}

// context is a browser of its own - in mode, or the page's choice (null).
async function context(mode = 'light') {
  const ctx = await browser.newContext({ ignoreHTTPSErrors: true, viewport: { width: 1280, height: 860 }, colorScheme: mode || 'light', reducedMotion: 'reduce' })
  if (mode) await ctx.addInitScript((m) => localStorage.setItem('janus-theme', m), mode)
  return ctx
}

// watch collects what goes wrong on the Controller's pages (Keycloak's
// own are its business).
function watch(page, where) {
  const ours = () => page.url().startsWith(CONTROLLER)
  page.on('pageerror', (e) => ours() && problems.push(`${where}: ${e.message}`))
  page.on('console', (m) => {
    if (ours() && (m.type() === 'error' || m.text().includes('[Report Only]'))) problems.push(`${where}: console ${m.type()}: ${m.text()}`)
  })
  page.on('response', (r) => {
    const u = r.url()
    if (u.startsWith(CONTROLLER) && r.status() >= 400 && !u.includes('/api/auth/status') && !u.includes('/api/auth/sso/finish')) problems.push(`${where}: ${r.status()} ${u}`)
  })
  return page
}

async function axe(page, where) {
  const res = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa']).analyze()
  for (const v of res.violations) {
    if (v.impact === 'serious' || v.impact === 'critical') problems.push(`${where}: axe ${v.impact} ${v.id} - ${v.nodes.map((n) => n.target.join(' ')).slice(0, 4).join(', ')}`)
  }
}

// totp is an authenticator app's code for a base32 secret (RFC 6238).
function totp(secret) {
  const bits = [...secret.replace(/=+$/, '')].map((c) => 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'.indexOf(c).toString(2).padStart(5, '0')).join('')
  const key = Buffer.from(bits.match(/.{8}/g).map((b) => parseInt(b, 2)))
  const counter = Buffer.alloc(8)
  counter.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)))
  const h = createHmac('sha1', key).update(counter).digest()
  return String((h.readUInt32BE(h[h.length - 1] & 0xf) & 0x7fffffff) % 1e6).padStart(6, '0')
}

async function settle(page) {
  await page.waitForSelector('.splash', { state: 'detached', timeout: 20000 }).catch(() => {})
  await page.evaluate(() => document.fonts.ready)
  await page.waitForTimeout(400)
}

// api calls the Controller with ctx's cookies.
async function api(ctx, method, path, data) {
  const r = await ctx.request.fetch(CONTROLLER + path, { method, data, failOnStatusCode: false })
  const text = await r.text()
  let json = null
  try {
    json = JSON.parse(text)
  } catch {
    // not JSON
  }
  return { status: r.status(), json, text }
}

async function status(ctx) {
  return (await api(ctx, 'GET', '/api/auth/status')).json
}

// signInThrough is user signing in through Keycloak in page, from the
// Controller's start (a route of the page): Keycloak's form filled, the
// page back.
async function signInThrough(page, user, start = '/') {
  await page.goto(CONTROLLER + start)
  await settle(page)
  await page.getByRole('button', { name: 'Sign in with Keycloak' }).click()
  await keycloakForm(page, user)
}

async function keycloakForm(page, user) {
  await page.waitForURL((u) => u.href.startsWith(KEYCLOAK), { timeout: 20000 })
  await page.locator('#username').fill(user)
  await page.locator('#password').fill(PASSWORDS[user])
  await page.locator('#kc-login').click()
  await page.waitForURL((u) => u.href.startsWith(CONTROLLER), { timeout: 20000 })
  // Finished: the page went back where it was - or says why not.
  await page.waitForFunction(() => !location.hash.startsWith('#/sso-done') || document.querySelector('.auth-box .error-box'), null, { timeout: 20000 })
  await settle(page)
}

// keycloak is Keycloak's admin REST API, as its bootstrap admin.
async function keycloak(method, path, body) {
  if (!keycloak.token) {
    const r = await fetch(`${KEYCLOAK}/realms/master/protocol/openid-connect/token`, {
      method: 'POST',
      body: new URLSearchParams({ grant_type: 'password', client_id: 'admin-cli', username: 'admin', password: KC_ADMIN_PASSWORD }),
    })
    check(r.ok, `Keycloak admin token: ${r.status}`)
    keycloak.token = (await r.json()).access_token
  }
  const r = await fetch(`${KEYCLOAK}/admin/realms/janus${path}`, {
    method,
    headers: { Authorization: `Bearer ${keycloak.token}`, ...(body ? { 'Content-Type': 'application/json' } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  })
  check(r.ok, `Keycloak ${method} ${path}: ${r.status}`)
  return r.status === 204 ? null : r.json()
}

const PROVIDER = {
  id: 'kc',
  name: 'Keycloak',
  preset: 'keycloak',
  enabled: true,
  issuer: `${KEYCLOAK}/realms/janus`,
  client_id: 'janus',
  client_secret: 'janus-sso-test-client-secret',
  scopes: ['openid', 'profile', 'email'],
  username_claim: 'preferred_username',
  groups_claim: 'groups',
  tls: { ca_pem: readFileSync(CA, 'utf8') },
  mfa: 'janus',
  rules: [
    { group: '/janus-admins', role: 'admin' },
    { group: '/janus-readers', role: 'reader' },
  ],
  sign_in_every_days: 7,
}

async function putProvider(admin, change) {
  const current = (await api(admin, 'GET', '/api/sso/providers')).json.providers.find((p) => p.id === 'kc')
  const body = { ...PROVIDER, version: current?.version || 0, ...change }
  if (current) delete body.client_secret // kept
  const r = await api(admin, 'PUT', '/api/sso/providers/kc', body)
  check(r.status === 200, `put the provider: ${r.status} ${r.text}`)
  return r.json
}

try {
  // --- set up: root, olga, the provider ---
  const admin = await context(null)
  let r = await api(admin, 'POST', '/api/auth/setup', { name: 'root', password: ROOT_PASSWORD, mfa_required: 'nobody' })
  check(r.status === 204, `setup: ${r.status} ${r.text}`)
  r = await api(admin, 'POST', '/api/users', { name: 'olga', role: 'operator', password: 'olga-given-password' })
  check(r.status === 201, `olga: ${r.status} ${r.text}`)
  const olga = await context()
  check((await api(olga, 'POST', '/api/auth/login', { name: 'olga', password: 'olga-given-password' })).status === 204, 'olga signs in')
  check((await api(olga, 'POST', '/api/auth/password', { current_password: 'olga-given-password', new_password: OLGA_PASSWORD })).status === 204, 'olga chooses hers')
  await putProvider(admin, {})
  r = await api(admin, 'POST', '/api/sso/test', { ...PROVIDER, client_secret: undefined })
  check(r.status === 200 && !r.json.error && r.json.issuer_matches && r.json.keys > 0, `test: ${r.text}`)
  step(`the Test button: ${r.json.keys} key(s), ${r.json.token_auth}, warnings: ${r.json.warnings.join(' | ') || 'none'}`)

  // --- the sign-in page: the button, in both modes ---
  for (const mode of ['light', 'dark']) {
    const ctx = await context(mode)
    const page = watch(await ctx.newPage(), `sign-in (${mode})`)
    await page.goto(CONTROLLER + '/')
    await settle(page)
    check(await page.getByRole('button', { name: 'Sign in with Keycloak' }).isVisible(), 'no Keycloak button')
    await axe(page, `sign-in (${mode})`)
    await page.screenshot({ path: `${OUT}/sign-in-${mode}.png` })
    await ctx.close()
  }

  // --- alice (/janus-admins), from janusctl's sign-in page: an admin,
  // back on that page, approving janusctl ---
  const key = 'ab'.repeat(32)
  const cliRoute = `/#/cli-login?port=4242&state=sso-test-state-0123&key=${key}`
  const alice = await context()
  const ap = watch(await alice.newPage(), 'alice')
  await signInThrough(ap, 'alice', cliRoute)
  check(new URL(ap.url()).hash === cliRoute.slice(1), `alice came back to ${ap.url()}, not janusctl's page`)
  let st = await status(alice)
  check(st.user?.name === 'alice' && st.user.role === 'admin' && st.user.provider === 'kc' && !st.user.has_password && st.user.sso?.provider === 'kc', `alice: ${JSON.stringify(st.user)}`)
  let callback = null
  await ap.route('http://127.0.0.1:4242/**', (route) => {
    callback = route.request().url()
    return route.fulfill({ status: 200, body: 'janusctl got it' })
  })
  await ap.getByRole('button', { name: 'Approve' }).click()
  await ap.waitForURL('http://127.0.0.1:4242/**', { timeout: 10000 })
  check(callback && new URL(callback).searchParams.get('code'), `janusctl's callback: ${callback}`)
  step('alice: an admin, made by the rules, back on janusctl’s page, which she approved')

  // --- signing out signs out of Keycloak too ---
  await ap.goto(CONTROLLER + '/')
  await settle(ap)
  const atKeycloak = ap.waitForRequest((q) => q.url().startsWith(`${KEYCLOAK}/realms/janus/protocol/openid-connect/logout`), { timeout: 20000 })
  await ap.getByRole('button', { name: 'Log out' }).click()
  const logout = new URL((await atKeycloak).url())
  check(logout.searchParams.get('id_token_hint') && logout.searchParams.get('post_logout_redirect_uri') === CONTROLLER + '/', `the sign-out at Keycloak: ${logout}`)
  await ap.waitForURL((u) => u.href.startsWith(CONTROLLER), { timeout: 20000 })
  await settle(ap)
  await ap.getByRole('button', { name: 'Sign in with Keycloak' }).waitFor({ timeout: 20000 })
  check(!(await status(alice)).authenticated, 'alice still signed in')
  await ap.getByRole('button', { name: 'Sign in with Keycloak' }).click()
  await ap.waitForURL((u) => u.href.startsWith(KEYCLOAK), { timeout: 20000 })
  check(await ap.locator('#username').isVisible(), 'Keycloak signed alice in again by itself: its session outlived the sign-out')
  step('signing out ended the Keycloak session too')
  await alice.close()

  // --- bob (/janus-readers): a reader ---
  const bob = await context()
  await signInThrough(watch(await bob.newPage(), 'bob'), 'bob')
  st = await status(bob)
  check(st.user?.name === 'bob' && st.user.role === 'reader', `bob: ${JSON.stringify(st.user)}`)
  await bob.close()
  step('bob: a reader')

  // --- carol (no group) and root (a local account's name): refused,
  // with a reference ---
  for (const [user, words] of [
    ['carol', 'no access'],
    ['root', 'already exists'],
  ]) {
    const ctx = await context()
    const page = await ctx.newPage()
    await signInThrough(page, user)
    const text = await page.locator('.auth-box').innerText()
    check(text.includes(words) && text.includes('Reference'), `${user}: ${text}`)
    check(!(await status(ctx)).authenticated, `${user} signed in`)
    if (user === 'carol') await page.screenshot({ path: `${OUT}/refused.png` })
    await ctx.close()
  }
  r = await api(admin, 'GET', '/api/users')
  check(!r.json.some((u) => u.name === 'carol') && r.json.find((u) => u.name === 'root').has_password, `accounts: ${r.text}`)
  step('carol (no group) and root (a local name) refused, with a reference')

  // --- olga links her Keycloak identity from Your account, then signs in
  // through it: olga, an operator still ---
  const op = watch(await olga.newPage(), 'olga')
  await op.goto(CONTROLLER + '/')
  await settle(op)
  await op.getByTitle('Your account').click()
  await op.getByRole('button', { name: 'Link Keycloak' }).click()
  await op.getByLabel(/Link an identity at Keycloak/).fill(OLGA_PASSWORD)
  await op.getByRole('button', { name: 'Continue at Keycloak' }).click()
  await keycloakForm(op, 'olga-kc')
  st = await status(olga)
  check(st.user?.name === 'olga' && st.user.identities?.length === 1 && st.user.identities[0].username === 'olga-kc', `olga: ${JSON.stringify(st.user)}`)
  await op.getByTitle('Your account').click()
  await op.waitForTimeout(300)
  await axe(op, 'olga account dialog')
  await op.screenshot({ path: `${OUT}/account-linked.png` })
  await olga.close()
  const olgaKC = await context()
  await signInThrough(watch(await olgaKC.newPage(), 'olga-kc'), 'olga-kc')
  st = await status(olgaKC)
  check(st.user?.name === 'olga' && st.user.role === 'operator' && st.user.has_password, `olga through Keycloak: ${JSON.stringify(st.user)}`)
  await olgaKC.close()
  step('olga linked olga-kc, then signed in through it: olga, an operator')

  // --- a group changed in Keycloak: alice a reader at her next sign-in;
  // then the Controller's rules changed: she's an operator at once ---
  const [kcAlice] = await keycloak('GET', '/users?username=alice&exact=true')
  const groups = await keycloak('GET', '/groups')
  const gid = (name) => groups.find((g) => g.name === name).id
  await keycloak('DELETE', `/users/${kcAlice.id}/groups/${gid('janus-admins')}`)
  await keycloak('PUT', `/users/${kcAlice.id}/groups/${gid('janus-readers')}`)
  const alice2 = await context()
  await signInThrough(watch(await alice2.newPage(), 'alice again'), 'alice')
  check((await status(alice2)).user?.role === 'reader', 'alice moved to /janus-readers is still an admin')
  await alice2.close()
  await putProvider(admin, {
    rules: [
      { group: '/janus-admins', role: 'admin' },
      { group: '/janus-readers', role: 'operator' },
    ],
  })
  r = await api(admin, 'GET', '/api/users')
  check(r.json.find((u) => u.name === 'alice').role === 'operator' && r.json.find((u) => u.name === 'bob').role === 'operator', `after the rules changed: ${r.text}`)
  step('a Keycloak group change applied at the next sign-in, a rules change at once')

  // --- the second factor: the Controller's by default (an enrolment
  // asked); the provider's when its ID token's acr is one asked ---
  // root sets an authenticator app up first: the policy covers it too.
  r = await api(admin, 'POST', '/api/auth/mfa/totp/setup')
  check(r.status === 200, `root's app: ${r.text}`)
  r = await api(admin, 'POST', '/api/auth/mfa/totp/enable', { code: totp(r.json.secret) })
  check(r.status === 200, `root's code: ${r.text}`)
  r = await api(admin, 'PUT', '/api/settings', { mfa_required: 'everyone' })
  check(r.status === 200, `policy: ${r.text}`)
  const bob2 = await context()
  await signInThrough(watch(await bob2.newPage(), 'bob, janus MFA'), 'bob')
  check(JSON.stringify((await status(bob2)).user?.needs) === '["mfa_enroll"]', 'bob: no enrolment asked')
  await bob2.close()
  await putProvider(admin, { mfa: 'provider', acr: ['1'] })
  const bob3 = await context()
  const b3 = watch(await bob3.newPage(), 'bob, provider MFA')
  await signInThrough(b3, 'bob')
  st = await status(bob3)
  check(st.user?.needs?.length === 0 && st.user.mfa?.by_provider, `bob with Keycloak's acr: ${JSON.stringify(st.user)}`)
  await b3.close() // its session is ended below, on purpose
  check((await api(admin, 'PUT', '/api/settings', { mfa_required: 'nobody' })).status === 200, 'policy back')
  step("the Controller's second factor asked by default; Keycloak's counted with acr 1")

  // --- the admin's card and form, both modes ---
  const rp = watch(await admin.newPage(), 'admin')
  for (const mode of ['light', 'dark']) {
    await rp.emulateMedia({ colorScheme: mode })
    await rp.goto(CONTROLLER + '/#/accounts')
    await rp.evaluate((m) => localStorage.setItem('janus-theme', m), mode)
    await rp.reload()
    await settle(rp)
    await rp.locator('.card-title', { hasText: 'Single sign-on' }).waitFor()
    await axe(rp, `accounts (${mode})`)
    await rp.screenshot({ path: `${OUT}/accounts-${mode}.png`, fullPage: true })
    await rp.locator('section.card', { hasText: 'OpenID Connect provider' }).getByRole('button', { name: 'Edit' }).click()
    await rp.screenshot({ path: `${OUT}/provider-top-${mode}.png` })
    await rp.getByRole('button', { name: 'Test' }).click()
    await rp.getByText('Discovery and').waitFor({ timeout: 20000 })
    await axe(rp, `provider form (${mode})`)
    await rp.locator('.modal').evaluate((m) => (m.scrollTop = m.scrollHeight))
    await rp.screenshot({ path: `${OUT}/provider-bottom-${mode}.png` })
    await rp.getByRole('button', { name: 'Cancel' }).click()
  }
  step('the admin card and form: both modes, axe')

  // --- disabled: its sessions end, no button; removed: its accounts
  // lapse ---
  await putProvider(admin, { enabled: false })
  check(!(await status(bob3)).authenticated, "bob's session outlived disabling the provider")
  st = await status(await context())
  check(st.sso.length === 0, `buttons: ${JSON.stringify(st.sso)}`)
  check((await api(admin, 'DELETE', '/api/sso/providers/kc')).status === 204, 'remove the provider')
  r = await api(admin, 'GET', '/api/users')
  check(r.json.find((u) => u.name === 'alice').sso?.lapsed, `alice after the provider's removal: ${r.text}`)
  step('disabled: sessions ended, no button; removed: its accounts lapsed')
} finally {
  await browser.close()
}

if (problems.length) {
  console.error(`sso: ${problems.length} problem(s):\n  ${problems.join('\n  ')}`)
  process.exit(1)
}
console.log('sso: all good')
