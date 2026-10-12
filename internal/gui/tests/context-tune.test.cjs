// Run with Node's test runner and Playwright on the module path; see README.md.
// Usage's Context tab tells each user, for each agent, the settings that
// would have spent the fewest tokens on their own calls (/api/tune), and
// puts one in on a click: Claude Code's auto-compact window for a model and
// its two cache lifetimes, Codex's compact limit. A setting already the
// leanest says so and has no button; one a variable comes before is told
// of and has none either; one magpie put in has Undo. Drawn before the
// gateway has read anything, in Chromium and WebKit, at a phone's width and
// a desk's, in every language, none of it wider than the page. Each row
// draws what every setting would have spent past the advice.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const curve = (lo, hi, best) => {
  const pts = [];
  for (let v = lo; v <= hi; v += 10000) pts.push({ value: v, cost: 1e9 + Math.abs(v - best) * 3000 });
  return pts;
};
const facts = { calls: 4100, streams: 60, compacts: 813, peak: 166000, after: 57000, growth: 1000, rework: 4700, quick: 0.95, pause: 0.045, away: 0.005, hour: 0.9 };
function tune(set) {
  const opus = set["claude/compact/claude-opus-5-5"];
  return {
    days: 7,
    agents: [
      { agent: "claude", calls: 4300, advice: [
        { knob: "compact", model: "claude-opus-5-5", setting: opus || { value: "300000", source: "settings" },
          compact: { model: "claude-opus-5-5", current: opus ? +opus.value : 300000, best: 160000, cost: 1.9e9, low: 1.46e9, curve: curve(110000, 300000, 160000), facts } },
        { knob: "compact", model: "claude-haiku-4-5-20251001-with-a-very-long-name", setting: {},
          compact: { model: "claude-haiku-4-5", current: 200000, best: 200000, cost: 2e8, low: 2e8, curve: curve(100000, 200000, 200000), facts: { ...facts, compacts: 0 } } },
        { knob: "cache_ttl", setting: { value: "1h", source: "env", locked: "CLAUDE_CODE_PROMPT_CACHE_TTL" },
          ttl: { current: "5m", best: "1h", cost: 5.5e8, low: 4.6e8, short: 5.5e8, hour: 4.6e8, facts } },
        { knob: "subagent_cache_ttl", setting: {},
          ttl: { current: "5m", best: "5m", cost: 9e7, low: 9e7, short: 9e7, hour: 1.2e8, facts } },
      ] },
      { agent: "codex", calls: 900, advice: [] },
    ],
  };
}

function serve(lang, posts) {
  const state = { agents: [{ id: "claude", name: "Claude Code", path: "/test/claude", fields: [] }, { id: "codex", name: "Codex", path: "/test/codex", fields: [] }], profiles: [], settings: { lang, theme: "light" } };
  const set = {};
  return async (r) => {
    const url = new URL(r.request().url());
    const json = (data) => r.fulfill({ json: data });
    if (url.pathname === "/boot.js") return r.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:true};` });
    if (url.pathname === "/wails/runtime.js") return r.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json(state);
    if (url.pathname === "/api/tune") return json(tune(set));
    if (url.pathname === "/api/tune/apply" || url.pathname === "/api/tune/undo") {
      const b = JSON.parse(r.request().postData());
      posts.push({ path: url.pathname, ...b });
      const key = [b.agent, b.knob, b.model].join("/");
      if (url.pathname.endsWith("apply")) set[key] = { value: b.value, source: "model", ours: true };
      else delete set[key];
      return json(set[key] || {});
    }
    // the gateway has read nothing yet
    if (url.pathname === "/api/context") return json({ days: 7, agents: [], sessions: [] });
    if (url.pathname === "/api/gateway/trace") {
      if (url.searchParams.get("wait")) return new Promise(() => {});
      return json({ mine: true, now: new Date().toISOString(), seq: 0, totals: { requests: 0, rerouted: 0, errors: 0 }, routes: [] });
    }
    if (url.pathname === "/api/providers") return json({ providers: [], presets: [], excluded: [], gateway: { running: true, window: true, url: "http://127.0.0.1:3999" } });
    if (url.pathname === "/api/usage/quotas") return json([]);
    if (url.pathname === "/api/groups") return json({ groups: [], models: [] });
    if (url.pathname === "/api/plugins") return json({ plugins: [] });
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    await r.fulfill({ body: await fs.readFile(file).catch(() => ""), contentType, status: 200 });
  };
}

const heading = { en: "Tuned for you", zh: "为你调优", "zh-TW": "為你調校", ja: "あなた向けの調整", de: "Für dich abgestimmt" };

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: the Context tab advises each agent's settings and puts one in`, async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    const errors = [];
    t.after(async () => {
      if (errors.length) console.log(errors);
      await browser.close();
    });
    for (const lang of Object.keys(heading)) for (const [width, height] of [[360, 760], [1100, 760]]) {
      await t.test(`${lang}, ${width}x${height}`, async () => {
        const ctx = await browser.newContext({ viewport: { width, height }, reducedMotion: "reduce" });
        await ctx.addInitScript(() => { try { localStorage.setItem("magpie.usageTab", "context"); } catch {} });
        const page = await ctx.newPage();
        page.setDefaultTimeout(5000);
        page.on("pageerror", (e) => errors.push(e.message));
        const posts = [];
        await page.route("**/*", serve(lang, posts));
        await page.goto("http://magpie.test/?view=usage");
        const rows = page.locator(".ctx-tune-row");
        await rows.first().waitFor();
        assert.equal(await page.getByText(heading[lang], { exact: true }).count(), 1, "the heading in " + lang);
        // Codex had too few calls: no card for it
        assert.equal(await page.locator(".ctx-tune").count(), 1);
        assert.equal(await rows.count(), 4);
        // only the window a change saves on has a button; the locked one says why
        const buttons = page.locator(".ctx-tune-row button");
        assert.equal(await buttons.count(), 1);
        assert.match(await rows.nth(0).locator(".ctx-tune-val.best b").innerText(), /160K/);
        assert.match(await rows.nth(2).locator(".ctx-tune-src").innerText(), /CLAUDE_CODE_PROMPT_CACHE_TTL/);
        assert.equal(await rows.nth(1).locator(".ctx-tune-val.best").count(), 0, "a window already the leanest shows no other");
        // the curve is what each setting spends past the advice: the window
        // set now says how much more, the advice sits on the level
        assert.equal(await rows.nth(0).locator(".ctx-tc-dot.now .ctx-tc-tag").innerText(), "+42%");
        assert.equal(await rows.nth(0).locator(".ctx-tc-dot.best").evaluate((e) => e.style.top), "84%");
        assert.equal(await rows.nth(2).locator(".ctx-tc-x span").count(), 2, "a cache lifetime's two choices");
        assert.ok(await rows.nth(0).locator(".ctx-tune-chart svg").isVisible());

        // nothing wider than the page, nor than its card
        const over = await page.evaluate(() => {
          const out = [];
          const view = document.querySelector("#view-usage");
          if (view.scrollWidth > view.clientWidth + 1) out.push("page " + view.scrollWidth + ">" + view.clientWidth);
          for (const c of document.querySelectorAll(".ctx-tune")) {
            const b = c.getBoundingClientRect();
            for (const e of c.querySelectorAll("*")) {
              const r = e.getBoundingClientRect();
              if (r.width && (r.right > b.right + 1 || r.left < b.left - 1)) out.push((e.className.baseVal ?? e.className) + " " + Math.round(r.left) + "–" + Math.round(r.right) + " of " + Math.round(b.left) + "–" + Math.round(b.right));
            }
          }
          return out;
        });
        assert.deepEqual(over, []);

        await buttons.first().evaluate((b) => b.click());
        await page.locator(".ctx-tune-row button", { hasText: { en: "Undo", zh: "撤销", "zh-TW": "復原", ja: "取り消す", de: "Rückgängig" }[lang] }).waitFor();
        assert.deepEqual(posts, [{ path: "/api/tune/apply", agent: "claude", knob: "compact", model: "claude-opus-5-5", value: "160000" }]);
        assert.match(await rows.nth(0).getAttribute("class"), /done/);
        await page.locator(".ctx-tune-row button").first().evaluate((b) => b.click());
        await page.locator(".ctx-tune-row button.primary").waitFor();
        assert.equal(posts[1].path, "/api/tune/undo");
        await ctx.close();
      });
    }
    assert.deepEqual(errors, []);
  });
}

// each curve draws itself once: the routing history answering after the
// advice, and a setting put in, redraw the pane, and what was on it already
// holds still
for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: the advice's curves play their entrance once`, async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const ctx = await browser.newContext({ viewport: { width: 1100, height: 900 } });
    await ctx.addInitScript(() => {
      try { localStorage.setItem("magpie.usageTab", "context"); } catch {}
      window.drawn = 0;
      document.addEventListener("animationstart", (e) => { if (e.animationName === "ctx-draw") window.drawn++; }, true);
    });
    const page = await ctx.newPage();
    page.setDefaultTimeout(5000);
    const errors = [];
    page.on("pageerror", (e) => errors.push(e.message));
    const posts = [];
    const base = serve("en", posts);
    let tuned;
    const answered = new Promise((res) => { tuned = res; });
    await page.route("**/*", async (r) => {
      const p = new URL(r.request().url()).pathname;
      // the history comes after the advice is drawn
      if (p === "/api/context") await answered.then(() => new Promise((res) => setTimeout(res, 300)));
      await base(r);
      if (p === "/api/tune") tuned();
    });
    await page.goto("http://magpie.test/?view=usage");
    await page.locator(".ctx-tune-chart svg").first().waitFor();
    const charts = await page.locator(".ctx-tune-chart svg").count();
    assert.ok(charts > 0);
    await page.locator(".ctx-none").waitFor(); // the history has answered and the pane is redrawn
    await page.waitForTimeout(300);
    assert.equal(await page.evaluate(() => window.drawn), charts, "after the history answered");
    await page.locator(".ctx-tune-row button.primary").first().evaluate((b) => b.click());
    await page.locator(".ctx-tune-row.done").first().waitFor();
    await page.waitForTimeout(300);
    assert.equal(await page.evaluate(() => window.drawn), charts, "after a setting was put in");
    assert.deepEqual(errors, []);
  });
}
