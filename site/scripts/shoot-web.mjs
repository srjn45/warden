// Capture the web GUI media for the docs site from a running ISOLATED demo instance.
//
//   bash site/scripts/demo-seed.sh up            # DEMO_ROOT/DEMO_PORT of your own, see its header
//   bash site/scripts/web-demo-extras.sh         # pipelines, run-owned worker, CPU load
//   sleep 300                                    # let the Metrics charts accumulate history
//   node site/scripts/shoot-web.mjs --url http://127.0.0.1:8809/ [--out DIR] [--only a,b] [--playwright DIR]
//   python3 -I site/scripts/optimize-web-media.py DIR   # quantize PNGs, assemble web-monitor.gif
//
// The target is given with --url (no WARDEN_* env, and unset WARDEN_TOKEN in the
// capturing shell: the demo daemon has no auth). Screenshots are of the real
// current web UI; the only page-side change is a text redaction that renders the
// demo checkout path (/tmp/wd-demo*/work, /tmp/warden-demo*/work) as ~/work.
//
// Outputs (in --out, default site/public/media):
//   web-cockpit.png           Cockpit: fleet card + autopilot-run project tree
//   web-cockpit-grouped.png   Cockpit scrolled to the other projects (roles, nesting, terminals)
//   web-autopilot-run.png     Autopilot run view (status, brain, plan task list, run-owned workers).
//                             The web has no separate plan detail/DAG view; the plan DAG is a TUI asset.
//   web-backends.png          Backends registry: tiers, default, enabled, limits
//   web-metrics.png           Metrics: CPU + memory charts
//   web-metrics-2.png         Metrics scrolled: Fast/Thinking brain telemetry + recent decisions
//   web-pipeline.png          Pipelines: list + layered fan-out DAG (checkout-hardening)
//   web-pipeline-job.png      Pipelines: webhook-retry-fix with the implement job's detail
//   frames/NNN-<ms>.png       web-monitor.gif source frames (Cockpit -> Pipelines -> Metrics)
import { mkdirSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';

const args = process.argv.slice(2);
const opt = (name, dflt) => {
  const i = args.indexOf(`--${name}`);
  return i >= 0 && args[i + 1] ? args[i + 1] : dflt;
};
const base = opt('url', '').replace(/\/+$/, '');
if (!base) { console.error('usage: node shoot-web.mjs --url http://127.0.0.1:PORT/ [--out DIR] [--only a,b] [--theme dark|light] [--playwright DIR]'); process.exit(2); }
const out = resolve(opt('out', 'site/public/media'));
const only = opt('only', '') ? new Set(opt('only').split(',')) : null;
const theme = opt('theme', 'dark');
const pwDir = opt('playwright', '');
const { chromium } = pwDir
  ? await import(pathToFileURL(join(resolve(pwDir), 'node_modules/playwright/index.mjs')).href)
  : await import('playwright');
mkdirSync(join(out, 'frames'), { recursive: true });

const browser = await chromium.launch();
const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 }, deviceScaleFactor: 1 });
// theme + path redaction apply to every page before first paint / on every DOM change
await ctx.addInitScript((t) => {
  try { localStorage.setItem('warden.theme', t); } catch { /* storage unavailable */ }
  const re = /\/tmp\/(?:wd|warden)-demo[^/\s]*\/work/g;
  const scrub = () => {
    const w = document.createTreeWalker(document.body || document, NodeFilter.SHOW_TEXT);
    for (let n = w.nextNode(); n; n = w.nextNode()) {
      if (re.test(n.nodeValue)) n.nodeValue = n.nodeValue.replace(re, '~/work');
      re.lastIndex = 0;
    }
  };
  new MutationObserver(scrub).observe(document, { childList: true, subtree: true, characterData: true });
  document.addEventListener('DOMContentLoaded', scrub);
}, theme);
const page = await ctx.newPage();
const want = (n) => !only || only.has(n);
const shot = async (name) => { await page.screenshot({ path: join(out, `${name}.png`) }); console.log('wrote', `${name}.png`); };
const open = async (path, settle = 2500) => {
  await page.goto(base + path, { waitUntil: 'networkidle' });
  await page.waitForTimeout(settle);
};
// Fail loudly instead of silently shooting a blank/wrong view.
const expectText = async (sel, needles, name) => {
  const t = await page.locator(sel).first().innerText({ timeout: 10000 });
  const miss = needles.filter((n) => !t.includes(n));
  if (miss.length) throw new Error(`${name}: ${sel} is missing ${JSON.stringify(miss)}`);
};
// The host's memory pressure chip is host-dependent; wait for the calm state.
const calm = async () => {
  for (let i = 0; i < 60; i++) {
    if (await page.getByText('pressure: normal').count()) return;
    await page.waitForTimeout(2000);
  }
  throw new Error('host pressure never settled to normal; retry when the machine is idle');
};
// the dashboard scrolls inside <main class="tab-content">, not the window
const scrollTab = async (y) => page.evaluate((top) => { document.querySelector('main.tab-content').scrollTop = top; }, y);

if (want('web-cockpit')) { await open('/cockpit', 4000); await calm(); await expectText('body', ['Fleet', 'api-gateway', 'AP:SSO rollout'], 'web-cockpit'); await shot('web-cockpit'); }
if (want('web-cockpit-grouped')) {
  await open('/cockpit', 4000); await calm();
  await page.evaluate(() => {
    const t = [...document.querySelectorAll('*')].find((e) => e.children.length === 0 && e.textContent.trim() === 'billing-svc');
    t?.scrollIntoView({ block: 'start' });
  });
  await page.waitForTimeout(1200);
  await shot('web-cockpit-grouped');
}
if (want('web-autopilot-run')) {
  await page.setViewportSize({ width: 1280, height: 900 });
  await open('/cockpit', 3000);
  await page.click('.autopilot-btn'); await page.waitForTimeout(1500);
  await expectText('.autopilot-panel', ['Autopilot', 'schema', 'login', 'docs', 'sso-callback'], 'web-autopilot-run');
  await shot('web-autopilot-run');
  await page.setViewportSize({ width: 1280, height: 800 });
}
if (want('web-backends')) {
  await open('/cockpit', 2000);
  await page.click('.backends-btn'); await page.waitForTimeout(1500);
  await expectText('.modal', ['Backends', 'claude', 'Subscription', 'Free'], 'web-backends');
  await shot('web-backends');
}
if (want('web-metrics') || want('web-metrics-2')) {
  await open('/metrics', 4000);
  await shot('web-metrics');
  await scrollTab(1560); await page.waitForTimeout(900);
  await expectText('main.tab-content', ['Fast / Thinking brain', 'Recent decisions'], 'web-metrics-2');
  await shot('web-metrics-2');
}
if (want('web-pipeline') || want('web-pipeline-job')) {
  await open('/pipelines', 2000);
  await page.locator('text=checkout-hardening').first().click(); await page.waitForTimeout(1500);
  await expectText('body', ['checkout-hardening', 'task-a', 'task-b', 'integrate'], 'web-pipeline');
  await shot('web-pipeline');
  await page.locator('text=webhook-retry-fix').first().click(); await page.waitForTimeout(1200);
  await page.locator('text=implement').first().click(); await page.waitForTimeout(1500);
  await expectText('body', ['Prompt', 'idempotent'], 'web-pipeline-job');
  await shot('web-pipeline-job');
}
if (want('web-monitor')) {
  // Cockpit -> Pipelines (two pipelines) -> Metrics scrolling through the charts. Each frame's hold
  // time is in its file name (NNN-<ms>.png); optimize-web-media.py assembles web-monitor.gif.
  let n = 0;
  const frame = async (ms) => { await page.screenshot({ path: join(out, 'frames', `${String(n++).padStart(3, '0')}-${ms}.png`) }); };
  await open('/cockpit', 3500); await calm(); await frame(2200);
  await page.click('a.tab[href="/pipelines"]'); await page.waitForTimeout(800);
  await page.locator('text=checkout-hardening').first().click(); await page.waitForTimeout(900); await frame(1800);
  await page.locator('text=webhook-retry-fix').first().click(); await page.waitForTimeout(900); await frame(1800);
  await page.click('a.tab[href="/metrics"]'); await page.waitForTimeout(1500); await frame(1600);
  await scrollTab(250); await page.waitForTimeout(600); await frame(1800);
  console.log('wrote', n, 'frames');
}
await browser.close();
