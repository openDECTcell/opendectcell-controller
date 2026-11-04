// SPDX-License-Identifier: AGPL-3.0-or-later
// usage: node crawl.js USER OUTDIR
const puppeteer = require('puppeteer-core');
const fs = require('fs');
const [user, out] = [process.argv[2], process.argv[3]];
const base = 'http://127.0.0.1:18080';
fs.mkdirSync(out, {recursive: true});
(async () => {
  const b = await puppeteer.launch({executablePath: process.env.CHROME || '/usr/bin/google-chrome', headless: 'new', args: ['--no-sandbox', '--window-size=1280,900']});
  const p = await b.newPage();
  await p.setViewport({width: 1280, height: 900});
  let cur = '';
  const problems = [];
  p.on('console', m => { if (['error', 'warning'].includes(m.type())) problems.push(`${cur}: console ${m.type()}: ${m.text()}`); });
  p.on('pageerror', e => problems.push(`${cur}: pageerror ${e.message}`));
  p.on('requestfailed', r => problems.push(`${cur}: request failed ${r.url()} ${r.failure()?.errorText}`));
  p.on('response', r => { if (r.status() >= 400 && r.url().startsWith(base)) problems.push(`${cur}: HTTP ${r.status()} ${r.url()}`); });
  await p.evaluateOnNewDocument(() => document.addEventListener('securitypolicyviolation', e => console.error('CSP violation: ' + e.violatedDirective + ' ' + e.blockedURI)));
  await p.goto(base + '/login');
  await p.type('input[name=username]', user);
  await p.type('input[name=password]', 'a long test password');
  await Promise.all([p.waitForNavigation(), p.click('button[type=submit]')]);
  if (p.url().includes('/login')) { const rc = fs.readFileSync(user + '.recovery', 'utf8').split('\n')[+(process.argv[4] || 0)]; await p.type('input[name=code]', rc); await Promise.all([p.waitForNavigation(), p.click('button[type=submit]')]); }
  const seen = new Set(), queue = ['/'];
  const pages = [];
  while (queue.length && seen.size < 80) {
    const u = queue.shift();
    if (seen.has(u) || u.startsWith('/logout') || u.startsWith('/static') || u.startsWith('/api/') || u.includes('/export')) continue;
    seen.add(u);
    cur = u;
    const r = await p.goto(base + u, {waitUntil: 'networkidle0'});
    const title = await p.title();
    const h1 = await p.$eval('h1, .page-title', e => e.textContent.trim()).catch(() => '');
    pages.push(`${r.status()} ${u}  [${title}] ${h1}`);
    await p.screenshot({path: `${out}/${u.replace(/[^a-z0-9]+/gi, '_') || 'root'}.png`, fullPage: true});
    const links = await p.$$eval('a[href]', as => as.map(a => a.getAttribute('href')));
    for (const l of links) if (l.startsWith('/') && !l.startsWith('//')) queue.push(l.split('#')[0]);
  }
  console.log(pages.join('\n'));
  console.log('--- problems (' + problems.length + ')');
  console.log([...new Set(problems)].join('\n'));
  await b.close();
})().catch(e => { console.error(e); process.exit(1); });
