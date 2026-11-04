// SPDX-License-Identifier: AGPL-3.0-or-later
const puppeteer = require('puppeteer-core'); const {code} = require('./totp'); const fs = require('fs');
const base = 'http://127.0.0.1:18080', out = 'out/flow'; fs.mkdirSync(out, {recursive: true});
const problems = [];
async function newPage(b) { const c = await b.createBrowserContext(); const p = await c.newPage(); await p.setViewport({width: 1280, height: 900});
  p.on('console', m => { if (['error','warning'].includes(m.type())) problems.push('console: ' + m.text()); });
  p.on('pageerror', e => problems.push('pageerror ' + e.message));
  await p.evaluateOnNewDocument(() => document.addEventListener('securitypolicyviolation', e => console.error('CSP violation: ' + e.violatedDirective + ' ' + e.blockedURI)));
  return p; }
async function login(p, user, pw = 'a long test password') { await p.goto(base + '/login'); await p.type('input[name=username]', user); await p.type('input[name=password]', pw); await Promise.all([p.waitForNavigation(), p.click('button[type=submit]')]); }
(async () => {
  const b = await puppeteer.launch({executablePath: process.env.CHROME || '/usr/bin/google-chrome', headless: 'new', args: ['--no-sandbox']});
  let p = await newPage(b);
  await login(p, 'admin');
  console.log('after login:', p.url());
  await p.goto(base + '/account/2fa'); await p.screenshot({path: out + '/1-2fa-admin-before.png'});
  await Promise.all([p.waitForNavigation(), p.click('form[action="/account/2fa/start"] button')]);
  await p.screenshot({path: out + '/2-setup.png', fullPage: true});
  const svg = await p.$('.qr svg'); console.log('QR svg present:', !!svg, svg ? JSON.stringify(await svg.boundingBox()) : '');
  const secret = await p.$eval('code.secret', e => e.textContent.trim()); console.log('secret length', secret.length);
  fs.writeFileSync('admin.secret', secret);
  await p.type('input[name=code]', code(secret));
  await Promise.all([p.waitForNavigation(), p.click('form[action="/account/2fa/confirm"] button')]);
  await p.screenshot({path: out + '/3-codes.png', fullPage: true});
  const codes = await p.$$eval('code.secret', es => es.map(e => e.textContent.trim())); console.log('recovery codes', codes.length);
  fs.writeFileSync('admin.recovery', codes.join('\n'));
  // same device again: logs out and in; the device was marked at setup?
  await p.goto(base + '/'); await Promise.all([p.waitForNavigation(), p.click('form[action="/logout"] button')]);
  await login(p, 'admin'); console.log('same device, after login:', p.url()); await p.screenshot({path: out + '/4-same-device.png'});
  // a new device (fresh context)
  const q = await newPage(b);
  await login(q, 'admin'); console.log('new device, after login:', q.url()); await q.screenshot({path: out + '/5-new-device.png', fullPage: true});
  console.log('remember box:', await q.$('input[type=checkbox]') ? 'yes' : 'no');
  await q.type('input[name=code]', code(secret, Date.now() + 30000));
  await Promise.all([q.waitForNavigation(), q.click('button[type=submit]')]); console.log('after code:', q.url());
  await q.goto(base + '/admin'); console.log('/admin:', q.url(), await q.title()); await q.screenshot({path: out + '/6-admin.png', fullPage: true});
  console.log('--- problems', [...new Set(problems)].join('\n'));
  await b.close();
})().catch(e => { console.error(e); process.exit(1); });
