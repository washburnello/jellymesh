import { chromium } from '/home/washburnello/.local/share/mise/installs/npm-playwright/latest/node_modules/playwright/index.mjs';
import fs from 'fs';

const BASE = 'http://127.0.0.1:18096';
const ITEM = '42569b6f90367519b288c964e1928886';
const STATS = '/home/washburnello/Work/jellymesh/lab/results/origin-stats.json';
const bytes = () => { try { return JSON.parse(fs.readFileSync(STATS,'utf8')).bytes_served; } catch { return 0; } };
const reqs  = () => { try { return JSON.parse(fs.readFileSync(STATS,'utf8')).requests; } catch { return 0; } };
const mark = (label, b0, r0) => console.log(`  [relay] ${label}: +${((bytes()-b0)/1e6).toFixed(2)} MB, +${reqs()-r0} requests`);

const browser = await chromium.launch({ headless: true, executablePath: '/usr/bin/chromium',
  args: ['--no-sandbox','--autoplay-policy=no-user-gesture-required'] });
const context = await browser.newContext({ viewport: { width: 1400, height: 900 } });
const page = await context.newPage();
const vids = [];
page.on('request', r => { const u=r.url(); if (u.includes('/Videos/')) vids.push(`${r.method()} ${u.replace(BASE,'').slice(0,120)}`); });

await page.goto(BASE, { waitUntil: 'domcontentloaded' });
await page.waitForTimeout(3500);
const f = await page.$('#txtManualName, input[name="username"]');
if (f) { await f.fill('labadmin'); const p=await page.$('input[type="password"]'); await p.fill('labpass12345');
  await page.keyboard.press('Enter'); await page.waitForTimeout(5000); }

console.log('STEP 1: open the movie');
let b0=bytes(), r0=reqs();
await page.goto(`${BASE}/web/#/details?id=${ITEM}`, { waitUntil: 'domcontentloaded' });
await page.waitForTimeout(6000);
await page.screenshot({ path: '/tmp/jmtest/02-details.png' });
mark('opening details page', b0, r0);

// Version picker
const selects = await page.$$('select');
for (const s of selects) {
  const opts = await s.$$eval('option', os => os.map(o => o.textContent.trim()));
  if (opts.some(o => o.includes('PeerOrigin') || o.includes('Local'))) {
    console.log('  VERSION PICKER options:', JSON.stringify(opts));
    await s.selectOption({ label: opts.find(o => o.includes('PeerOrigin')) });
    console.log('  selected the PeerOrigin (remote) version');
  }
}
await page.waitForTimeout(1500);

console.log('STEP 2: press play');
b0=bytes(); r0=reqs();
const playBtn = await page.$('.btnPlay, button[title="Play"], .detailButton-icon');
if (playBtn) { await playBtn.click(); } else { await page.keyboard.press('Enter'); }
await page.waitForTimeout(12000);
await page.screenshot({ path: '/tmp/jmtest/03-playing.png' });
mark('12s of playback', b0, r0);

const state = await page.evaluate(() => {
  const v = document.querySelector('video');
  if (!v) return { video: false };
  return { video: true, currentTime: +v.currentTime.toFixed(2), duration: +(v.duration||0).toFixed(2),
           paused: v.paused, readyState: v.readyState, networkState: v.networkState,
           videoWidth: v.videoWidth, videoHeight: v.videoHeight, err: v.error ? v.error.code : null, src: (v.currentSrc||'').slice(0,110) };
});
console.log('  VIDEO STATE:', JSON.stringify(state));

console.log('STEP 3: seek to 120s');
b0=bytes(); r0=reqs();
await page.evaluate(() => { const v=document.querySelector('video'); if (v) v.currentTime = 120; });
await page.waitForTimeout(9000);
const afterSeek = await page.evaluate(() => { const v=document.querySelector('video');
  return v ? { currentTime:+v.currentTime.toFixed(2), paused:v.paused, readyState:v.readyState, err: v.error?v.error.code:null } : null; });
console.log('  AFTER SEEK:', JSON.stringify(afterSeek));
await page.screenshot({ path: '/tmp/jmtest/04-after-seek.png' });
mark('seek to 120s', b0, r0);

console.log('--- /Videos/ requests observed ---');
[...new Set(vids)].slice(0,10).forEach(v => console.log('  ', v));
await browser.close();
