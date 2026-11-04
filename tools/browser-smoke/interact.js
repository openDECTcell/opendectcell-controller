// SPDX-License-Identifier: AGPL-3.0-or-later
const puppeteer = require('puppeteer-core'); const fs = require('fs');
const base = 'http://127.0.0.1:18080', out = 'out/interact'; fs.mkdirSync(out, {recursive: true});
const problems = [];
async function newPage(b) { const c = await b.createBrowserContext(); const p = await c.newPage(); await p.setViewport({width: 1280, height: 900});
  p.on('console', m => { if (['error','warning'].includes(m.type())) problems.push('console: ' + m.text()); });
  p.on('pageerror', e => problems.push('pageerror ' + e.message));
  await p.evaluateOnNewDocument(() => document.addEventListener('securitypolicyviolation', e => console.error('CSP violation: ' + e.violatedDirective + ' ' + e.blockedURI)));
  return p; }
async function login(p, user, rc) { await p.goto(base + '/login'); await p.type('input[name=username]', user); await p.type('input[name=password]', 'a long test password'); await Promise.all([p.waitForNavigation(), p.click('button[type=submit]')]);
  if (rc) { await p.type('input[name=code]', rc); await Promise.all([p.waitForNavigation(), p.click('button[type=submit]')]); } }
const ok = (c, m) => console.log((c ? 'PASS ' : 'FAIL ') + m);
(async () => {
  const b = await puppeteer.launch({executablePath: process.env.CHROME || '/usr/bin/google-chrome', headless: 'new', args: ['--no-sandbox']});
  const a = await newPage(b); await login(a, 'anna');
  // handset + station: tick disclose and save
  await a.goto(base + '/handsets'); const hl = await a.$eval('a[href^="/handsets/"]:not([href="/handsets/register"])', e => e.getAttribute('href'));
  await a.goto(base + hl); await a.click('input[name=disclosed]');
  await Promise.all([a.waitForNavigation(), a.click('form button[type=submit].btn-primary')]);
  await a.goto(base + hl); ok(await a.$eval('input[name=disclosed]', e => e.checked), 'handset disclosure saved and shown checked');
  await a.goto(base + '/stations'); const sl = await a.$eval('a[href^="/stations/"]', e => e.getAttribute('href'));
  await a.goto(base + sl); await a.click('input[name=disclosed]');
  const saves = await a.$$('button[type=submit].btn-primary'); await Promise.all([a.waitForNavigation(), saves[saves.length - 1].click()]);
  await a.goto(base + sl); ok(await a.$eval('input[name=disclosed]', e => e.checked), 'station disclosure saved and shown checked');
  const r404 = await a.goto(base + '/admin'); ok(r404.status() === 404, 'anna on /admin: ' + r404.status());
  // admin sees them under anna
  const ad = await newPage(b); await login(ad, 'admin', fs.readFileSync('admin.recovery', 'utf8').split('\n')[1]);
  await ad.goto(base + '/admin/users'); const al = await ad.$$eval('a[href^="/admin/users/"]', es => es.map(e => [e.textContent.trim(), e.getAttribute('href')]));
  const anna = al.find(x => x[0] === 'anna')[1]; const ben = al.find(x => x[0] === 'ben')[1];
  await ad.goto(base + anna); let t = await ad.evaluate(() => document.body.innerText); await ad.screenshot({path: out + '/admin-anna.png', fullPage: true});
  ok(t.includes('Annas A540') && t.includes('Annas lab'), 'admin sees anna\'s disclosed handset and station under her account');
  await ad.goto(base + ben); t = await ad.evaluate(() => document.body.innerText); ok(t.includes('Bens C430') && t.includes('Bens flat'), 'admin sees ben\'s (seeded as disclosed)');
  await ad.goto(base + '/handsets'); t = await ad.evaluate(() => document.body.innerText); ok(!t.includes('Annas A540') && !t.includes('Bens C430'), 'admin\'s own handset list has nobody else\'s handsets');
  await ad.goto(base + '/cells'); t = await ad.evaluate(() => document.body.innerText); ok(t.includes('Annas lab') && t.includes('Bens flat'), 'admin cell list shows all cells'); await ad.screenshot({path: out + '/admin-cells.png', fullPage: true});
  // sessions: a second browser of anna's is listed and can be ended from the first
  const a2 = await newPage(b); await login(a2, 'anna');
  await a.goto(base + '/account/sessions'); await a.screenshot({path: out + '/sessions.png', fullPage: true});
  let st = await a.evaluate(() => document.body.innerText); ok(st.includes('this one') && (st.match(/Chrome|Headless/g) || []).length >= 2, 'sessions page lists this browser and the other one');
  await Promise.all([a.waitForNavigation(), a.click('form[action="/account/sessions/end"] button')]);
  await a2.goto(base + '/account'); ok(a2.url().includes('/login'), 'the ended session is logged out');
  // ben: erase the account
  const be = await newPage(b); await login(be, 'ben'); await be.goto(base + '/account/erase'); await be.type('input[name=password]', 'a long test password'); await be.click('input[name=understand]');
  await Promise.all([be.waitForNavigation(), be.click('button.btn-danger')]); console.log('after erase:', be.url()); await be.screenshot({path: out + '/after-erase.png'});
  await login(be, 'ben'); ok(be.url().includes('/login'), 'ben cannot log in any more'); 
  await ad.goto(base + '/admin/users'); t = await ad.evaluate(() => document.body.innerText); ok(!t.includes('ben'), 'ben is gone from the user list');
  console.log('--- problems:', [...new Set(problems)].join(' | ') || 'none'); await b.close();
})().catch(e => { console.error(e); process.exit(1); });
