// Part of capture_quick_connect.py (JS-037): what jellyfin-web 12.4 sends and shows for Quick Connect. With PHASE=on a
// browser with no token picks "Use Quick Connect", a viewer approves its code on the Quick Connect page, an
// administrator approves another's, and a refused Initiate is answered with each status in turn. With PHASE=off it
// records the login page and the Quick Connect page once the server has Quick Connect turned off. Prints a JSON object
// on stdout. Run from a directory with playwright-core.
import { createRequire } from "node:module";
const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");

const { BASE: base, PHASE: phase, PASSWORD: password } = process.env;
const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH ?? "/usr/bin/chromium", args: ["--disable-gpu"] });
const result = {};
const sent = [];

// The Quick Connect requests of one browser. A token and a secret are written as their presence alone.
async function browserAs(tag) {
  const context = await browser.newContext({ viewport: { width: 1100, height: 900 } });
  const page = await context.newPage();
  page.on("response", async (response) => {
    const url = new URL(response.url());
    if (!/^\/(QuickConnect\/|Users\/AuthenticateWithQuickConnect)/i.test(url.pathname)) return;
    const request = response.request();
    const authorization = request.headers().authorization ?? "";
    const query = Object.fromEntries([...url.searchParams].map(([key, value]) => [key, /secret/i.test(key) ? "<secret>" : value]));
    sent.push({
      browser: tag, at: Date.now(), method: request.method(), path: url.pathname, query,
      authorization_scheme: authorization.split(" ")[0], sends_token: /Token="[^"]+"/.test(authorization),
      other_token_headers: Object.keys(request.headers()).filter((name) => /token|x-emby/i.test(name)),
      body: request.postData()?.replace(/[0-9A-F]{32,}/gi, "<secret>") ?? null, status: response.status(),
    });
  });
  return { context, page };
}

const shown = (page) => page.evaluate(() => {
  const visible = (element) => !!element && !element.classList.contains("hide") && element.offsetParent !== null;
  const button = document.querySelector(".btnQuick");
  return {
    route: location.hash.split("?")[0],
    quick_connect_button: visible(button) ? button.innerText.trim() : null,
    dialogs: [...document.querySelectorAll(".toast, .formDialog")].map((e) => e.innerText.trim()).filter(Boolean),
    page_text: (document.querySelector(".mainAnimatedPage:not(.hide)")?.innerText ?? "").trim().slice(0, 600),
    sign_in_form: visible(document.querySelector("#txtManualName")),
    user_select: [...document.querySelectorAll("select#userId")].map((select) => ({ selected: select.selectedOptions[0]?.textContent, options: [...select.options].map((o) => o.textContent) })),
    code_input: !!document.querySelector("#txtQuickConnectCode"),
    quick_connect_links: [...document.querySelectorAll("a[href*='quickconnect' i]")].filter(visible).map((a) => a.innerText.trim()),
  };
});

async function loginPage(tag) {
  const opened = await browserAs(tag);
  await opened.page.goto(`${base}/web/`);
  await opened.page.waitForSelector("#txtManualName", { timeout: 60000 });
  await opened.page.waitForTimeout(3000); // The page redraws its form once as the client finishes starting.
  return opened;
}

async function signedIn(username) {
  const opened = await loginPage(username);
  await opened.page.fill("#txtManualName", username);
  await opened.page.fill("#txtManualPassword", password);
  await opened.page.click("button[type=submit]");
  await opened.page.waitForFunction(() => !/login|wizard/.test(location.hash) && !!window.ApiClient?.accessToken(), null, { timeout: 30000 });
  await opened.page.waitForTimeout(1500);
  return opened;
}

async function quickConnectPage(page) {
  await page.goto(`${base}/web/#/quickconnect`);
  await page.waitForSelector("#txtQuickConnectCode", { timeout: 30000 });
  await page.waitForTimeout(2000);
  return shown(page);
}

async function submitCode(page, code) {
  await page.fill("#txtQuickConnectCode", code);
  await page.click("#quickConnectPreferencesPage button[type=submit]");
  await page.waitForTimeout(3000);
  return shown(page);
}

async function startQuickConnect(page) {
  await page.click(".btnQuick");
  await page.waitForSelector(".formDialog", { timeout: 15000 });
  await page.waitForTimeout(1000);
  const state = await shown(page);
  return { state, code: /\b(\d{6})\b/.exec(state.dialogs.join(" "))?.[1] };
}

async function menu(page) {
  await page.goto(`${base}/web/#/mypreferencesmenu`);
  await page.waitForTimeout(4000);
  return shown(page);
}

if (phase === "on") {
  const tv = await loginPage("tv");
  result.login_page = await shown(tv.page);
  const started = await startQuickConnect(tv.page);
  result.login_dialog = { ...started.state, dialogs: started.state.dialogs.map((text) => text.replace(/\d{6}/, "<code>")) };
  await tv.page.waitForTimeout(12000);
  const polls = sent.filter((row) => row.browser === "tv" && /Connect$/i.test(row.path) && /quickconnect/i.test(row.path)).map((row) => row.at);
  result.poll_seconds = polls.slice(1).map((at, i) => Math.round((at - polls[i]) / 1000));

  const viewer = await signedIn("viewer");
  result.viewer_menu = await menu(viewer.page);
  result.viewer_page = await quickConnectPage(viewer.page);
  result.viewer_unknown_code = await submitCode(viewer.page, "000000");
  result.viewer_approved = await submitCode(viewer.page, `${started.code.slice(0, 3)} ${started.code.slice(3)}`);
  await tv.page.waitForFunction(() => /home/.test(location.hash) && !!window.ApiClient?.accessToken(), null, { timeout: 30000 });
  await tv.page.waitForTimeout(1500);
  result.tv_after = { ...(await shown(tv.page)), signed_in_as: await tv.page.evaluate(async () => (await window.ApiClient.getCurrentUser()).Name) };

  const second = await loginPage("tv2");
  const again = await startQuickConnect(second.page);
  const admin = await signedIn("admin");
  result.admin_menu = await menu(admin.page);
  result.admin_page = await quickConnectPage(admin.page);
  result.admin_approved = await submitCode(admin.page, again.code);
  await second.page.waitForFunction(() => /home/.test(location.hash) && !!window.ApiClient?.accessToken(), null, { timeout: 30000 });
  result.tv2_signed_in_as = await second.page.evaluate(async () => (await window.ApiClient.getCurrentUser()).Name);

  // What the login page shows when Initiate is refused, answered here in the server's place.
  result.refused_initiate = [];
  for (const [status, retryAfter] of [[401, null], [403, null], [403, "60"], [429, null], [429, "60"], [500, null], [503, null], [503, "60"]]) {
    const refused = await loginPage(`refused-${status}`);
    const headers = { "content-type": "text/plain" };
    if (retryAfter) headers["retry-after"] = retryAfter;
    await refused.page.route(/\/quickconnect\/initiate/i, (route) => route.fulfill({ status, headers, body: "refused for the capture" }));
    await refused.page.click(".btnQuick");
    await refused.page.waitForTimeout(3000);
    const state = await shown(refused.page);
    result.refused_initiate.push({ status, retry_after: retryAfter, dialogs: state.dialogs, sign_in_form: state.sign_in_form });
    await refused.context.close();
  }
} else if (phase === "refused-approval") {
  // JS-037: jellyscale answers 409 where Jellyfin's second approval answers 500. This is what a viewer sees for
  // each. A fresh context per status, a signed-in viewer on #/quickconnect, and the Authorize request answered in
  // the server's place so the page sees each status. Rerun with:
  //   PHASE=refused-approval BASE=http://127.0.0.1:<port> PASSWORD=<password> node capture_quick_connect_web.mjs
  result.refused_approve = [];
  for (const status of [409, 500, 404, 403]) {
    const opened = await signedIn("viewer");
    await opened.page.route(/\/quickconnect\/authorize/i, (route) => route.fulfill({
      status, headers: { "content-type": "text/plain" }, body: "Error processing request.",
    }));
    const before = await quickConnectPage(opened.page);
    const after = await submitCode(opened.page, "123456");
    await opened.page.waitForTimeout(1000);
    let start = 0;
    while (start < before.page_text.length && before.page_text[start] === after.page_text[start]) start++;
    let end = 0;
    while (end < before.page_text.length - start &&
      before.page_text[before.page_text.length - 1 - end] === after.page_text[after.page_text.length - 1 - end]) end++;
    result.refused_approve.push({
      status,
      page_text_added: after.page_text.slice(start, after.page_text.length - end).trim(),
      dialogs: after.dialogs,
      route: after.route,
      still_signed_in: await opened.page.evaluate(() => !!window.ApiClient?.accessToken()),
    });
    await opened.context.close();
  }
} else {
  const tv = await loginPage("off-tv");
  result.login_page = await shown(tv.page);
  const viewer = await signedIn("viewer");
  result.viewer_menu = await menu(viewer.page);
  await viewer.page.goto(`${base}/web/#/quickconnect`);
  await viewer.page.waitForTimeout(5000);
  result.viewer_page = await shown(viewer.page);
  if (result.viewer_page.code_input) result.viewer_submit = await submitCode(viewer.page, "123456");
}

await browser.close();
// The requests jellyfin-web sent, without their times, and with the repeats of a poll once.
const seen = new Set();
result.requests = sent.map(({ at, ...row }) => row).filter((row) => {
  const key = JSON.stringify(row);
  if (seen.has(key)) return false;
  seen.add(key);
  return true;
});
console.log(JSON.stringify(result));
