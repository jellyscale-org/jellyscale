// Opens a local page holding a <video> whose src redirects, and reports the result.
// Usage: node chromium.mjs <page.html>
import { createRequire } from "node:module";

const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");
const browser = await chromium.launch({
  executablePath: "/usr/bin/chromium",
  args: ["--autoplay-policy=no-user-gesture-required", "--disable-gpu"],
});
const page = await browser.newPage();
await page.goto(`file://${process.argv[2]}`);
await page.waitForFunction(() => window.r?.done, null, { timeout: 30000 }).catch(() => {});
console.log(JSON.stringify(await page.evaluate(() => window.r)));
await browser.close();
