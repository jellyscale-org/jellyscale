// JS-033, D-100: what jellyfin-web 12.4 shows when an empty password is answered 400. Against a disposable Jellyfin 12.4
// whose startup is done, with the administrator in ADMIN_NAME and ADMIN_PASSWORD, one library, and no user "observed"
// but the one this makes; the 400 is given here in the server's place (Jellyfin itself takes an empty password).
// Usage, from a directory with playwright-core in node_modules:
//   BASE=http://127.0.0.1:<port> ADMIN_NAME=... ADMIN_PASSWORD=... node capture_users_empty_password_web.mjs
import { createRequire } from "node:module";
const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");
const { BASE: base, ADMIN_NAME, ADMIN_PASSWORD } = process.env;
const observed = "Qx7-observed";
const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH ?? "/usr/bin/chromium", args: ["--disable-gpu"] });
async function signIn(name, pw) {
  const page = await (await browser.newContext({ viewport: { width: 1280, height: 900 } })).newPage();
  page.setDefaultTimeout(15000);
  await page.goto(`${base}/web/`);
  await page.waitForSelector("#txtManualName", { state: "attached" }); await page.waitForTimeout(1500);
  if (!(await page.isVisible("#txtManualName"))) await page.click(".btnManual");
  await page.fill("#txtManualName", name); await page.fill("#txtManualPassword", pw); await page.click("form.manualLoginForm button[type=submit]");
  await page.waitForURL(/#\/home/); await page.waitForTimeout(2000);
  return page;
}
const admin = await signIn(ADMIN_NAME, ADMIN_PASSWORD);
await admin.evaluate(async (pw) => window.ApiClient.ajax({ type: "POST", url: window.ApiClient.getUrl("Users/New"), data: JSON.stringify({ Name: "observed", Password: pw }), contentType: "application/json" }), observed);
const id = await admin.evaluate(async () => (await window.ApiClient.getUsers()).find((u) => u.Name === "observed").Id);
async function observe(page, label, hash, act) {
  await page.goto("about:blank"); await page.goto(`${base}/web/#/${hash}`); await page.waitForTimeout(3500);
  const sent = [];
  const handler = async (route) => {
    const request = route.request();
    if (request.method() !== "POST" || !/\/users\/(new|password|[^/]+\/password)/i.test(new URL(request.url()).pathname)) return route.fallback();
    sent.push(`${new URL(request.url()).pathname.replace(/[0-9a-f]{32}/, "ID")} ${request.postData()}`);
    await route.fulfill({ status: 400, contentType: "text/plain", body: "REASON-MARKER the password is commonly guessed" });
  };
  await page.route("**/*", handler);
  await act(page);
  const seen = new Set(); let state;
  for (let i = 0; i < 16; i++) {
    state = await page.evaluate(() => {
      const visible = (e) => e.getClientRects().length > 0;
      return { texts: [...document.querySelectorAll(".toast, .dialogContainer .dialog, [role=dialog], [role=alert], .MuiSnackbar-root, .MuiAlert-root")].filter(visible).map((e) => e.innerText.trim().replace(/\s+/g, " ")).filter(Boolean),
        spinner: [...document.querySelectorAll(".docspinner.mdlSpinnerActive, .MuiCircularProgress-root")].some(visible),
        marker: document.body.innerText.includes("REASON-MARKER"), invalid: [...document.querySelectorAll("input:invalid")].filter(visible).map((e) => e.id) };
    });
    state.texts.forEach((t) => seen.add(t));
    await page.waitForTimeout(250);
  }
  await page.unroute("**/*", handler);
  console.log(JSON.stringify({ label, sent, seen: [...seen], spinner: state.spinner, serverReasonShown: state.marker, invalidFields: state.invalid, hash: await page.evaluate(() => location.hash.replace(/[0-9a-f]{32}/, "ID")) }));
  await page.keyboard.press("Escape");
}
const save = (page, name) => page.getByRole("button", { name }).filter({ visible: true }).first().click();
const fill = async (page, current) => { if (current !== null) await page.fill("#txtCurrentPassword", current); await page.fill("#txtNewPassword", ""); await page.fill("#txtNewPasswordConfirm", ""); await save(page, /^Save Password$/); };
await observe(admin, "new user, empty password", "dashboard/users/add", async (p) => { await p.fill("#txtUsername", "never-made"); await save(p, /^Save$/); });
await observe(admin, "another user's password page, empty new password", `dashboard/users/${id}/password`, (p) => fill(p, null));
const viewer = await signIn("observed", observed);
await observe(viewer, "own password page, empty new password, right current", `userprofile?userId=${id}`, (p) => fill(p, observed));
await browser.close();
