// Part of capture_users_admin.py (JS-033): every request jellyfin-web's user list, new-user, user profile, access,
// parental-control, password and Quick Connect pages send, what their Save posts with no change, and what they show when
// the answer is 400 or 403 (given here in the server's place, so the server is not changed). Prints JSON on stdout.
// Run from a directory with playwright-core.
import { createRequire } from "node:module";
const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");

const env = process.env;
const base = env.BASE;
const target = env.TARGET_ID;
const browser = await chromium.launch({ executablePath: env.CHROMIUM_PATH ?? "/usr/bin/chromium", args: ["--disable-gpu"] });
const pages = [];
const refusals = [];

const device = 'MediaBrowser Client="capture", Device="capture", DeviceId="capture-web-observer", Version="1"';
async function api(method, path, token, body, as = device) {
  const authorization = token ? `${as}, Token="${token}"` : as;
  const headers = { authorization, "content-type": "application/json" };
  const reply = await fetch(base + path, { method, headers, body: body && JSON.stringify(body) });
  const text = await reply.text();
  if (!reply.ok) throw new Error(`${method} ${path}: ${reply.status} ${text.slice(0, 200)}`);
  return /^[[{]/.test(text) ? JSON.parse(text) : text;
}
const adminToken = (await api("POST", "/Users/AuthenticateByName", null, { Username: env.ADMIN_NAME, Pw: env.ADMIN_PASSWORD })).AccessToken;

const redact = (path) => path.replace(/(api_?key=)[^&]+/gi, "$1<token>").replace(/([?&]code=)[^&]+/gi, "$1<code>");

async function signIn(name, password) {
  const context = await browser.newContext({ viewport: { width: 1200, height: 900 } });
  const page = await context.newPage();
  page.setDefaultTimeout(20000);
  await page.goto(`${base}/web/index.html#/login`);
  await page.waitForSelector("#txtManualName", { state: "attached", timeout: 60000 });
  await page.waitForTimeout(1000);
  if (!(await page.isVisible("#txtManualName"))) await page.click(".btnManual");
  await page.fill("#txtManualName", name);
  await page.fill("#txtManualPassword", password);
  await page.click("form.manualLoginForm button[type=submit]");
  await page.waitForURL(/#\/home/).catch(async () => {
    throw new Error(`sign-in of ${name} did not reach home, at ${page.url()}: ${(await page.innerText("body")).replace(/\s+/g, " ").slice(0, 300)}`);
  });
  await page.waitForTimeout(3000);

  const log = { sent: [], errors: [] };
  page.on("pageerror", (error) => log.errors.push(error.message.slice(0, 200)));
  page.on("response", (response) => {
    const request = response.request();
    const url = new URL(request.url());
    if (url.origin !== base || url.pathname.startsWith("/web")) return;
    const path = redact(url.pathname + url.search);
    log.sent.push({ method: request.method(), path, status: response.status(), body: request.postData() ?? undefined });
  });
  return { context, page, log };
}

// Away to a blank page first, so the page is built again and asks again.
async function open(page, hash) {
  await page.goto("about:blank");
  await page.goto(`${base}/web/index.html#/${hash}`);
  await page.waitForTimeout(3500);
}

// Toasts and snackbars come and go, so look repeatedly and keep every text seen.
async function watch(page, milliseconds = 3500) {
  const seen = new Set();
  let busy = false;
  let open = [];
  for (let waited = 0; waited < milliseconds; waited += 250) {
    const now = await page.evaluate(() => {
      const visible = (element) => element.getClientRects().length > 0;
      const selector = ".toast, .dialogContainer .dialog, [role=dialog], [role=alert], .MuiSnackbar-root, .MuiAlert-root";
      return {
        texts: [...document.querySelectorAll(selector)].filter(visible).map((element) => element.innerText.trim().replace(/\s+/g, " ")).filter(Boolean),
        busy: [...document.querySelectorAll(".docspinner.mdlSpinnerActive, .MuiCircularProgress-root")].some(visible),
      };
    }).catch(() => ({ texts: [], busy: false }));
    now.texts.forEach((text) => seen.add(text.slice(0, 300)));
    busy = now.busy;
    open = now.texts.map((text) => text.slice(0, 80));
    await page.waitForTimeout(250);
  }
  return { messages: [...seen], still_shown_at_the_end: open, spinner_at_the_end: busy };
}

const dialogButton = (page, name) => page.locator(".dialogContainer button, [role=dialog] button").filter({ hasText: name }).last();
const save = (page) => page.getByRole("button", { name: /^Save$/ }).filter({ visible: true }).first().click();

function unique(requests) {
  const seen = new Set();
  return requests.filter((request) => {
    const key = `${request.method} ${request.path} ${request.status} ${request.body}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}

const accounts = { administrator: [env.ADMIN_NAME, env.ADMIN_PASSWORD], viewer: [env.VIEWER_NAME, env.VIEWER_PASSWORD] };

async function record(pageName, as, stepsOf) {
  const session = await signIn(...accounts[as]);
  const steps = stepsOf(session);
  const done = [];
  for (const [step, act] of steps) {
    session.log.sent.length = 0;
    let note;
    let shows;
    try {
      note = await act(session.page);
      shows = await watch(session.page);
    } catch (error) {
      shows = { capture_failed: error.message.split("\n")[0] };
    }
    done.push({ step, requests: unique(session.log.sent), shows: { ...shows, ...note, hash: await session.page.evaluate(() => location.hash) } });
  }
  pages.push({ page: pageName, as, steps: done });
  await session.context.close();
}

function changed(before, after) {
  const keys = [...new Set([...Object.keys(before), ...Object.keys(after)])].sort();
  return {
    not_posted: keys.filter((key) => !(key in after)),
    only_posted: keys.filter((key) => !(key in before)),
    differing: Object.fromEntries(keys.filter((key) => key in before && key in after && JSON.stringify(before[key]) !== JSON.stringify(after[key]))
      .map((key) => [key, [before[key], after[key]]])),
  };
}

// Save with nothing changed: what each posted Policy or user body differs in from what GET answered before.
function saveUnchanged(session) {
  return async (page) => {
    const before = await api("GET", `/Users/${target}`, adminToken);
    await save(page);
    await page.waitForTimeout(2000);
    const compared = {};
    for (const request of session.log.sent.filter((sent) => sent.method === "POST" && sent.body)) {
      const body = JSON.parse(request.body);
      if (/\/policy$/i.test(request.path)) compared.policy_against_get = changed(before.Policy, body);
      if (/^\/users\?/i.test(request.path)) compared.user_against_get = changed(before, body);
    }
    return compared;
  };
}

async function refuse(as, pageName, action, hash, method, pattern, act) {
  for (const status of [400, 403]) {
    const session = await signIn(...accounts[as]);
    const { page, log } = session;
    let refused = null;
    const matches = (url) => pattern.test(url.pathname + url.search);
    const handler = async (route) => {
      const request = route.request();
      if (request.method() !== method) return route.fallback();
      const url = new URL(request.url());
      refused = `${method} ${redact(url.pathname + url.search)}`;
      const answer = status === 403
        ? { contentType: "application/json", body: '"There must be at least one user in the system with administrative access."' }
        : { contentType: "text/plain", body: "Error processing request." };
      await route.fulfill({ status, ...answer });
    };
    if (act) await open(page, hash);
    await page.route(matches, handler);
    log.sent.length = 0;
    log.errors.length = 0;
    let shows;
    try {
      if (act) await act(page);
      else {
        await page.goto("about:blank");
        await page.goto(`${base}/web/index.html#/${hash}`);
      }
      shows = await watch(page, act ? 3500 : 9000);
      if (!act) shows.page_text = (await page.evaluate(() => [...document.querySelectorAll(".mainAnimatedPage:not(.hide), main")].map((element) => element.innerText).join(" "))).replace(/\s+/g, " ").slice(0, 200);
    } catch (error) {
      shows = { capture_failed: error.message.split("\n")[0] };
    }
    await page.unroute(matches, handler);
    const index = log.sent.findIndex((request) => `${request.method} ${request.path}` === refused);
    const after = unique(log.sent.slice(index + 1)).map((request) => `${request.method} ${request.path.split("?")[0]} ${request.status}`);
    const ofPage = unique(log.sent).filter((request) => /^\/users(\?|\/|$)/i.test(request.path)).map((request) => `${request.method} ${request.path} ${request.status}`);
    refusals.push({ page: pageName, action, refused, status, shows, hash: await page.evaluate(() => location.hash), after: act ? after.filter((line) => !/BitrateTest|System\/Endpoint/.test(line)) : [], user_requests: act ? undefined : ofPage, errors: [...new Set(log.errors)] });
    await session.context.close();
  }
}

async function fillPasswords(page, current, next) {
  if (current !== null) await page.fill("#txtCurrentPassword", current);
  await page.locator("#txtNewPassword").fill(next);
  await page.locator("#txtNewPasswordConfirm").fill(next);
  await page.getByRole("button", { name: /Save Password|^Save$/ }).filter({ visible: true }).first().click();
}

async function resetPassword(page) {
  await page.getByRole("button", { name: /Reset Password/ }).filter({ visible: true }).first().click();
  await page.waitForTimeout(700);
  await dialogButton(page, /Reset|OK|Yes/).click();
}

async function deleteUser(page, name) {
  await page.locator(".cardBox").filter({ hasText: name }).locator(".btnUserMenu").click();
  await page.waitForTimeout(700);
  await page.locator(".actionSheetMenuItem").filter({ hasText: "Delete" }).click();
  await page.waitForTimeout(700);
  await dialogButton(page, /Delete|OK|Yes/).click();
}

async function addUser(page, name, password) {
  await page.fill("#txtUsername", name);
  await page.fill("#txtPassword", password);
  await save(page);
}

async function authorize(page, code) {
  await page.fill("#txtQuickConnectCode", code);
  await page.getByRole("button", { name: /Authorize/ }).click();
}

// A device of its own: authorizing a code signs that device in, which ends the device's earlier tokens.
const quickConnectCode = async () => (await api("POST", "/QuickConnect/Initiate", null, undefined, device.replace("capture-web-observer", "capture-web-tv"))).Code;
const edit = (tab) => `dashboard/users/${target}/${tab}`;

await record("user list", "administrator", () => [
  ["load", (page) => open(page, "dashboard/users")],
  ["a card's menu", async (page) => {
    await page.locator(".btnUserMenu").first().click();
    await page.waitForTimeout(700);
    const menu = await page.locator(".actionSheetMenuItem").allInnerTexts();
    await page.keyboard.press("Escape");
    return { menu };
  }],
]);
await record("new user", "administrator", () => [
  ["load", (page) => open(page, "dashboard/users/add")],
  ["Save with a name and a password, defaults left", (page) => addUser(page, "capture-web-made", env.NEW_PASSWORD)],
  ["load again", (page) => open(page, "dashboard/users/add")],
  ["Save with a name, no password, one library ticked instead of all", async (page) => {
    await page.locator("label").filter({ hasText: /^\s*Movies\s*$/, visible: true }).click();
    await addUser(page, "capture-web-restricted", "");
  }],
]);
await record("user list: delete", "administrator", () => [
  ["load", (page) => open(page, "dashboard/users")],
  ["Delete from a card's menu, confirmed", (page) => deleteUser(page, "capture-web-restricted")],
]);
for (const [pageName, tab] of [["user profile", "profile"], ["access", "access"], ["parental control", "parentalcontrol"]]) {
  await record(pageName, "administrator", (session) => [
    ["load", (page) => open(page, edit(tab))],
    ["Save with no change", saveUnchanged(session)],
  ]);
}
await record("password (another user)", "administrator", () => [
  ["load", async (page) => {
    await open(page, edit("password"));
    return { current_password_field: await page.locator("#txtCurrentPassword").isVisible() };
  }],
  ["Save Password", (page) => fillPasswords(page, null, env.NEW_PASSWORD)],
  ["Reset Password, confirmed", resetPassword],
]);
await record("Quick Connect", "administrator", () => [
  ["load", async (page) => {
    await open(page, "quickconnect");
    return { user_choices: await page.locator("select#userId option").allInnerTexts() };
  }],
  ["Authorize a real code", async (page) => authorize(page, await quickConnectCode())],
]);

await refuse("administrator", "user list", "load", "dashboard/users", "GET", /^\/users(\?|$)/i);
await refuse("administrator", "user list", "delete", "dashboard/users", "DELETE", /^\/users\//i, (page) => deleteUser(page, "capture-web-made"));
await refuse("administrator", "new user", "Save", "dashboard/users/add", "POST", /^\/users\/new/i, (page) => addUser(page, "capture-web-refused", env.NEW_PASSWORD));
await refuse("administrator", "new user", "Save, the Policy that follows", "dashboard/users/add", "POST", /\/policy$/i, async (page) => {
  await addUser(page, `capture-web-half-${refusals.length}`, env.NEW_PASSWORD);
});
await refuse("administrator", "user profile", "load", edit("profile"), "GET", new RegExp(`^/users/${target}`, "i"));
await refuse("administrator", "user profile", "Save, the user", edit("profile"), "POST", /^\/users\?/i, save);
await refuse("administrator", "user profile", "Save, the Policy", edit("profile"), "POST", /\/policy$/i, save);
await refuse("administrator", "access", "Save", edit("access"), "POST", /\/policy$/i, save);
await refuse("administrator", "parental control", "Save", edit("parentalcontrol"), "POST", /\/policy$/i, save);
await refuse("administrator", "password (another user)", "Save Password", edit("password"), "POST", /^\/users\/(password|[^/]+\/password)/i, (page) => fillPasswords(page, null, env.NEW_PASSWORD));
await refuse("administrator", "password (another user)", "Reset Password", edit("password"), "POST", /^\/users\/(password|[^/]+\/password)/i, resetPassword);
await refuse("administrator", "Quick Connect", "Authorize", "quickconnect", "POST", /^\/quickconnect\/authorize/i, (page) => authorize(page, "000000"));
const own = `userprofile?userId=${env.VIEWER_ID}`;
await refuse("viewer", "own profile", "Save Password", own, "POST", /^\/users\/(password|[^/]+\/password)/i, (page) => fillPasswords(page, env.VIEWER_PASSWORD, env.NEW_PASSWORD));
await refuse("viewer", "own profile", "Reset Password", own, "POST", /^\/users\/(password|[^/]+\/password)/i, resetPassword);
await refuse("viewer", "Quick Connect", "Authorize", "quickconnect", "POST", /^\/quickconnect\/authorize/i, (page) => authorize(page, "000000"));
await record("own profile and password", "viewer", () => [
  ["load", (page) => open(page, own)],
  ["Save Password with a wrong current password (the server's real answer)", (page) => fillPasswords(page, "wrong", env.NEW_PASSWORD)],
  ["load again", (page) => open(page, own)],
  ["Save Password with the right current password", (page) => fillPasswords(page, env.VIEWER_PASSWORD, env.NEW_PASSWORD)],
  ["load once more", (page) => open(page, own)],
  ["Reset Password, confirmed", resetPassword],
]);
// The reset above left the viewer without a password.
await api("POST", `/Users/Password?userId=${env.VIEWER_ID}`, adminToken, { NewPw: env.VIEWER_PASSWORD });
await record("Quick Connect", "viewer", () => [
  ["load", (page) => open(page, "quickconnect")],
  ["Authorize an unknown code (the server's real answer)", (page) => authorize(page, "000000")],
  ["load again", (page) => open(page, "quickconnect")],
  ["Authorize a real code", async (page) => authorize(page, await quickConnectCode())],
]);
await browser.close();
console.log(JSON.stringify({ pages, refusals }));
