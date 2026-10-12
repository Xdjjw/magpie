// Run with Node's test runner and Playwright on the module path; see README.md.
// Settings → Privacy → Subscription accounts (#1535, MaxHaiCom): whether
// magpie reads the agents' own sign-ins (Claude Code's and Codex's files,
// the Keychain) at all. On by default; Off posts
// settings/account-discovery {off:true} and is drawn chosen, a save of
// another setting keeps it, On takes it back. With MAGPIE_NO_ACCOUNTS set
// the row says so and shows Off. The row fits a narrow window without the
// page scrolling sideways, and no click moves the page. Every language; no
// backend, the API is faked here.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");

function settingsPayload(over) {
  return {
    theme: "light", lang: "en", tray: "panel", quotaLeft: false, currency: "usd",
    dock: false, dockWindow: false, proxy: "", redact: false, redactPersonal: false, redactWords: [],
    codexWarmup: "", claudeWarmup: "", codexWarmAt: "", claudeWarmAt: "", workbuddyCheckin: false, noStats: false,
    trayUsage: "", trayUsages: [], trayUsageEvery: 3, vision: "", imageGen: "",
    version: "0.1.400", dir: "~/.config/magpie", gateway: "http://127.0.0.1:3425",
    proxyNow: "none", proxySource: "none", login: false,
    visionModels: [], imageGenModels: [], workbuddyCheckins: [], lanURLs: [],
    fx: { rate: 7.2, at: new Date().toISOString(), stale: false },
    ...over,
  };
}

function server(lang, posts, over) {
  let cur = settingsPayload({ lang, ...over });
  return async (route) => {
    const req = route.request(), url = new URL(req.url());
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:false};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light" }, fx: cur.fx });
    if (url.pathname === "/api/settings/account-discovery") {
      const body = req.postDataJSON();
      posts.push(body);
      cur = { ...cur, noAccountDiscovery: body.off || undefined };
      return json(cur);
    }
    if (url.pathname === "/api/settings") {
      // the save of every other setting leaves this one as it is (api.go)
      if (req.method() === "POST") cur = { ...cur, ...req.postDataJSON(), noAccountDiscovery: cur.noAccountDiscovery };
      return json(cur);
    }
    if (url.pathname === "/api/usage/quotas") return json([]);
    if (url.pathname === "/api/plugins") return json({ plugins: [] });
    if (url.pathname === "/api/groups") return json({ groups: [], models: [] });
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    await route.fulfill({ body: await fs.readFile(file), contentType });
  };
}

const view = (page) => page.locator("#view-settings").evaluate((v) => v.scrollTop);

async function open(browser, lang, width, posts, over) {
  const errors = [];
  const context = await browser.newContext({ viewport: { width, height: 900 }, reducedMotion: "reduce" });
  const page = await context.newPage();
  page.setDefaultTimeout(5000);
  page.on("pageerror", (e) => errors.push(e.message));
  await page.route("**/*", server(lang, posts, over));
  await page.goto("http://magpie.test/?view=usage");
  await page.locator("#prefs").click();
  await page.locator(":is(#setTab-privacy, .nav-s[data-of=setTab-privacy]):visible").click();
  await page.locator("#discoveryList .opt").first().waitFor();
  return { context, page, errors };
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(engine + ": agents' sign-ins read, or not at all", async (t) => {
    assert(["chromium", "webkit"].includes(engine), "BROWSER must be chromium or webkit");
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());

    for (const [lang, head, name] of [
      ["en", "Agents' sign-ins", "Subscription accounts"],
      ["zh", "智能体登录", "订阅账号"],
      ["zh-TW", "智慧體登入", "訂閱帳號"],
      ["ja", "エージェントのサインイン", "サブスクリプションアカウント"],
      ["de", "Anmeldungen der Agenten", "Abo-Konten"],
    ]) {
      for (const width of [900, 360]) {
        await t.test(lang + " " + width, async () => {
          const posts = [];
          const { context, page, errors } = await open(browser, lang, width, posts);
          const box = page.locator("#discoveryList");
          const opts = box.locator(".opt");
          assert.equal((await page.locator("#setPage-privacy .row-head .label").last().textContent()).trim(), head);
          assert.equal((await box.locator(".name").textContent()).trim(), name);
          const sub = (await box.locator(".sub").textContent()).trim();
          assert(sub.includes("Claude Code") && sub.includes("Codex"), "the sub names what is read: " + sub);
          if (lang !== "en") assert(!sub.startsWith("Read the accounts"), lang + " sub not translated");
          // the same Off and On as Mask secrets above it
          assert.deepEqual(await opts.allTextContents(), await page.locator("#redactList .row").first().locator(".opt").allTextContents());
          assert.equal(await opts.nth(1).evaluate((b) => b.classList.contains("on")), true, "On by default");

          const row = box.locator(".row");
          await row.scrollIntoViewIfNeeded();
          await page.waitForTimeout(300);
          const fit = await row.evaluate((r) => {
            const b = r.getBoundingClientRect(), segs = r.querySelector(".val").getBoundingClientRect();
            return { right: b.right, segsRight: segs.right, vw: document.documentElement.clientWidth,
              sideways: document.scrollingElement.scrollWidth > document.documentElement.clientWidth };
          });
          assert(fit.segsRight <= fit.vw + 0.5 && fit.right <= fit.vw + 0.5, "the row runs out of the window: " + JSON.stringify(fit));
          assert.equal(fit.sideways, false, "the page scrolls sideways");

          const before = await view(page);
          await opts.nth(0).click();
          for (let i = 0; i < 50 && !posts.length; i++) await page.waitForTimeout(50);
          assert.deepEqual(posts.at(-1), { off: true });
          await page.waitForFunction(() => document.querySelectorAll("#discoveryList .opt")[0]?.classList.contains("on"));
          await page.waitForTimeout(200);
          assert.equal(await view(page), before, "Off moved the page");

          // another setting saved keeps it
          await page.locator("#redactList .opt").nth(1).evaluate((b) => b.click());
          await page.waitForTimeout(300);
          assert.equal(await page.locator("#discoveryList .opt").nth(0).evaluate((b) => b.classList.contains("on")), true,
            "a save of another setting turned discovery back on");

          await page.locator("#discoveryList .opt").nth(1).click();
          for (let i = 0; i < 50 && posts.length < 2; i++) await page.waitForTimeout(50);
          assert.deepEqual(posts.at(-1), { off: false });
          await page.waitForFunction(() => document.querySelectorAll("#discoveryList .opt")[1]?.classList.contains("on"));
          assert.deepEqual(errors, []);
          if (process.env.ARTIFACT_DIR) {
            await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
            await page.waitForTimeout(500); // the pages drawn again after the save
            await row.scrollIntoViewIfNeeded();
            await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `${engine}-account-discovery-${lang}-${width}.png`) });
          }
          await context.close();
        });
      }
      // MAGPIE_NO_ACCOUNTS: off for this run, and the row says why
      await t.test(lang + " env", async () => {
        const { context, page, errors } = await open(browser, lang, 900, [], { noAccountsEnv: true });
        const box = page.locator("#discoveryList");
        assert.equal(await box.locator(".opt").nth(0).evaluate((b) => b.classList.contains("on")), true, "Off under MAGPIE_NO_ACCOUNTS");
        assert((await box.locator(".sub").textContent()).includes("MAGPIE_NO_ACCOUNTS"));
        assert.deepEqual(errors, []);
        await context.close();
      });
    }
  });
}
