// Usage: node site/scripts/shoot-web.mjs
// Requires the warden web GUI running locally (see README "Web GUI"), default :4321 dev or the daemon's embedded build.
import { chromium } from 'playwright';
const URL = process.env.WARDEN_WEB_URL || 'http://localhost:4321/';
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1280, height: 800 } });
await page.goto(URL, { waitUntil: 'networkidle' });
await page.screenshot({ path: 'site/public/media/web-overview.png' });
await browser.close();
console.log('wrote site/public/media/web-overview.png');
