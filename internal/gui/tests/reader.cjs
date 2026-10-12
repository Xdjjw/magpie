// The app deliberately undoes unsolicited scrollIntoView (including Playwright's
// automatic scroll). Bring a control into sight with real wheel input first.
// Wholly in sight: one cut off by a fraction of a pixel at the view's foot
// was scrolled the rest of the way by Playwright's click itself, before the
// app saw the click, and the page moved 1px under it (860 to 861 on the
// Usage page's Fetch from archive, once the archive switch in its header
// had moved the rows 4px down).
async function inView(page, locator) {
  await locator.waitFor({state:"visible"});
  if (await locator.evaluate(e => !e.closest("#view-usage"))) return;
  const view = page.locator("#view-usage");
  const v = await view.boundingBox();
  await page.mouse.move(v.x + v.width / 2, v.y + v.height / 2);
  for (let i = 0; i < 80; i++) {
    const b = await locator.boundingBox();
    if (!b) { await page.waitForTimeout(40); continue; }
    // one taller than the view: its top in sight
    if (b.y >= v.y && (b.y + b.height <= v.y + v.height || (b.height > v.height && b.y < v.y + v.height))) {
      await page.waitForTimeout(300);
      return;
    }
    const delta = b.y < v.y ? -20 : 20;
    await page.mouse.wheel(0, delta);
    await page.waitForTimeout(40);
  }
  throw new Error("control did not come into sight: " + JSON.stringify({view:v,target:await locator.boundingBox(),scroll:await view.evaluate(e=>e.scrollTop)}));
}

async function click(page, locator) {
  await inView(page, locator);
  await locator.click();
}

// Settings is drawn from each read of the settings, and opening the page
// reads them twice: the page's own read, then the window's load() once the
// state is in. Every row is replaced by the second draw, so a row found
// after the first is detached under whatever the test does to it next
// (#1653: scrollIntoViewIfNeeded in WebKit on a busy machine). Call
// watchSettingsReads before page.goto; settingsDrawn then waits until load()
// has asked for its read and every read asked for has been drawn. A page is
// watched once however many times it is opened.
const watched = new WeakSet();
async function watchSettingsReads(page) {
  if (watched.has(page)) return;
  watched.add(page);
  await page.addInitScript(() => {
    const reads = window.__settingsReads = { asked: 0, drawn: 0 };
    const fetch0 = window.fetch;
    window.fetch = function (input, init) {
      const p = fetch0.apply(this, arguments);
      const url = new URL(typeof input === "string" ? input : input.url, location.href);
      if (url.pathname !== "/api/settings" || (init?.method || "GET") !== "GET") return p;
      reads.asked++;
      // drawn a task after its body is in: loadSettings draws in the
      // microtasks that follow
      const drawn = () => setTimeout(() => reads.drawn++);
      return p.then((res) => {
        const text = res.text;
        res.text = function () { return text.call(this).finally(drawn); };
        return res;
      }, (e) => { drawn(); throw e; });
    };
  });
}

// load.done is set in the same run of load() that then asks for the
// settings, with no wait between, so once it is set the read is counted.
const settingsDrawn = (page) => page.waitForFunction(() => load.done && __settingsReads.asked === __settingsReads.drawn);

module.exports = { inView, click, watchSettingsReads, settingsDrawn };
