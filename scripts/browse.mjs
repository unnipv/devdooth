// Minimal Playwright CLI over a Devdooth CDP endpoint.
//
//   node scripts/browse.mjs <endpoint> <command> [args...]
//
// Commands: goto <url> | title | text <sel> | eval <expr> | click <sel> |
//           fill <sel> <text> | press <key> [sel] | links [n] | info |
//           screenshot <path>
//
// Each run connects to the leased browser, acts, and disconnects. The remote
// browser and its page persist, so consecutive invocations share state. The
// connection is deliberately not closed with browser.close(), which over CDP
// would close the remote browser.
import { chromium } from 'playwright-core';

const [endpoint, command, ...args] = process.argv.slice(2);
if (!endpoint || !command) {
  console.error('usage: node scripts/browse.mjs <endpoint> <command> [args...]');
  process.exit(2);
}

const browser = await chromium.connectOverCDP(endpoint);
const context = browser.contexts()[0] ?? (await browser.newContext());
const page = context.pages()[0] ?? (await context.newPage());
let exit = 0;

try {
  switch (command) {
    case 'goto': {
      const target = args[0];
      if (!target) throw new Error('goto needs a url');
      await page.goto(target, { waitUntil: 'domcontentloaded', timeout: 45000 });
      console.log(`url: ${page.url()}`);
      break;
    }
    case 'title':
      console.log(await page.title());
      break;
    case 'text': {
      const value = await page.textContent(args[0], { timeout: 15000 });
      console.log((value ?? '').replace(/\s+/g, ' ').trim());
      break;
    }
    case 'eval':
      console.log(String(await page.evaluate(args[0])));
      break;
    case 'click':
      await page.click(args[0], { timeout: 15000 });
      console.log('clicked');
      break;
    case 'fill':
      await page.fill(args[0], args.slice(1).join(' '), { timeout: 15000 });
      console.log('filled');
      break;
    case 'press':
      if (args[1]) {
        await page.press(args[1], args[0], { timeout: 15000 });
      } else {
        await page.keyboard.press(args[0]);
      }
      console.log('pressed');
      break;
    case 'links': {
      const limit = Number(args[0] || 25);
      const links = await page.$$eval('a', (as) => as.map((a) => [a.innerText.trim(), a.href]));
      for (const [text, href] of links.filter(([t]) => t).slice(0, limit)) {
        console.log(`${text} | ${href}`);
      }
      break;
    }
    case 'info': {
      const info = await page.evaluate(() => ({
        url: location.href,
        title: document.title,
        platform: navigator.platform,
        ua: navigator.userAgent,
        cores: navigator.hardwareConcurrency,
      }));
      console.log(JSON.stringify(info, null, 2));
      break;
    }
    case 'screenshot': {
      const target = args[0] || 'screenshot.png';
      const mode = args[1] || 'full';
      await page.setViewportSize({ width: 1280, height: 900 });
      await page.screenshot({ path: target, fullPage: mode !== 'view' });
      console.log(`saved ${target}`);
      break;
    }
    default:
      console.error(`unknown command: ${command}`);
      exit = 2;
  }
} catch (err) {
  console.error(`error: ${err.message}`);
  exit = 1;
}

process.exit(exit);
