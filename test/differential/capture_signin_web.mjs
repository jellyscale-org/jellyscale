// Part of capture_signin.py (JS-145): what jellyfin-web shows when sign-in is answered with each status, with and
// without Retry-After. It opens the web client of the Jellyfin at $BASE, types a name and a password into the form, and
// answers POST /Users/AuthenticateByName itself. Prints a JSON array on stdout. Run from a directory with playwright-core.
import { createRequire } from "node:module";
const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");

const base = process.env.BASE;
const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH ?? "/usr/bin/chromium", args: ["--disable-gpu"] });
const rows = [];
for (const [status, retryAfter] of [[401, null], [401, "60"], [403, null], [403, "60"], [429, null], [429, "60"], [503, null], [503, "60"]]) {
  const context = await browser.newContext({ viewport: { width: 1000, height: 800 } });
  const page = await context.newPage();
  const headers = { "content-type": "text/plain" };
  if (retryAfter) headers["retry-after"] = retryAfter;
  await page.route(/\/users\/authenticatebyname/i, (route) => route.fulfill({ status, headers, body: "refused for the capture" }));
  await page.goto(`${base}/web/index.html#/login.html`);
  await page.waitForSelector("#txtManualName", { timeout: 60000 });
  await page.fill("#txtManualName", "nobody");
  await page.fill("#txtManualPassword", "secret");
  await page.click("button[type=submit]");
  await page.waitForTimeout(2500);
  const shown = await page.evaluate(() => {
    const texts = [...document.querySelectorAll(".toast, .dialog, .dialogContainer, .dialogContent")].map((e) => e.innerText.trim()).filter(Boolean);
    return { texts, onForm: !!document.querySelector("#txtManualName") && document.querySelector("#txtManualName").offsetParent !== null, hash: location.hash };
  });
  rows.push({ status, retry_after: retryAfter, kind: shown.texts.some((t) => /Connection Failure|unable to connect/i.test(t)) ? "dialog" : "toast", shows: shown.texts[0] ?? "", on_sign_in_form: shown.onForm, hash: shown.hash });
  await context.close();
}
await browser.close();
console.log(JSON.stringify(rows));
