import { chromium } from '/home/washburnello/.local/share/mise/installs/npm-playwright/latest/node_modules/playwright/index.mjs';

const BASE = 'http://127.0.0.1:18096';
const browser = await chromium.launch({ headless: true, executablePath: '/usr/bin/chromium', args: ['--no-sandbox','--autoplay-policy=no-user-gesture-required'] });
const context = await browser.newContext({ viewport: { width: 1400, height: 900 } });
const page = await context.newPage();

const netlog = [];
page.on('request', r => {
  const u = r.url();
  if (u.includes('/Videos/') || u.includes('PlaybackInfo') || u.includes('/Items/')) {
    netlog.push(`${r.method()} ${u.replace(BASE,'')}`.slice(0, 160));
  }
});
page.on('console', m => { if (m.type() === 'error') netlog.push('CONSOLE-ERR ' + m.text().slice(0,120)); });

await page.goto(BASE, { waitUntil: 'domcontentloaded' });
await page.waitForTimeout(4000);
console.log('TITLE:', await page.title());
console.log('URL:', page.url());

// Log in through the real form if present
const userField = await page.$('#txtManualName, input[name="username"]');
if (userField) {
  console.log('LOGIN: manual form found');
  await userField.fill('labadmin');
  const pw = await page.$('#txtManualPassword, input[type="password"]');
  if (pw) await pw.fill('labpass12345');
  const btn = await page.$('button[type="submit"], .btnSubmit');
  if (btn) await btn.click();
  await page.waitForTimeout(5000);
} else {
  const card = await page.$('.cardBox, .listItem');
  if (card) { console.log('LOGIN: user card found, clicking'); await card.click(); await page.waitForTimeout(3000);
    const pw = await page.$('input[type="password"]');
    if (pw) { await pw.fill('labpass12345'); await page.keyboard.press('Enter'); await page.waitForTimeout(5000); }
  } else console.log('LOGIN: no obvious login UI');
}
console.log('URL after login:', page.url());
await page.screenshot({ path: '/tmp/jmtest/01-after-login.png' });
console.log('--- network ---');
netlog.slice(-15).forEach(l => console.log(' ', l));
await browser.close();
