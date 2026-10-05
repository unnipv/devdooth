// Connects ordinary Playwright to a Devdooth lease over CDP and drives a page.
//
//   node scripts/e2e-playwright.mjs 'ws://coordinator/v1/lease/lse_x/cdp?token=...'
//
// This is the acceptance check for Milestone 0: a normal Playwright client,
// unmodified, controlling a browser leased from a Devdooth worker.
import { chromium } from 'playwright-core';

const endpoint = process.argv[2] || process.env.DEVDOOTH_ENDPOINT;
if (!endpoint) {
  console.error('usage: node scripts/e2e-playwright.mjs <cdp-endpoint>');
  process.exit(2);
}

const expected = 'devdooth-playwright';
let browser;
try {
  browser = await chromium.connectOverCDP(endpoint);

  // A CDP-attached browser exposes its default context and page.
  const context = browser.contexts()[0] ?? (await browser.newContext());
  const page = context.pages()[0] ?? (await context.newPage());

  await page.goto(`data:text/html,<title>${expected}</title><h1 id="msg">hello from devdooth</h1>`);
  await page.waitForSelector('#msg');

  const title = await page.title();
  const text = await page.textContent('#msg');
  const version = browser.version();

  console.log(`browser   : ${version}`);
  console.log(`title     : ${title}`);
  console.log(`text      : ${text}`);

  if (title !== expected) throw new Error(`unexpected title: ${title}`);
  if (text !== 'hello from devdooth') throw new Error(`unexpected text: ${text}`);
  console.log('OK: Playwright connected over CDP through Devdooth');
} catch (err) {
  console.error('FAIL:', err.message);
  process.exitCode = 1;
} finally {
  if (browser) await browser.close().catch(() => {});
}
