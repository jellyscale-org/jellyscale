// Part of capture_subtitles_default.py (JS-041): which SubtitleLanguagePreference the real jellyfin-web 12.4 Subtitles page
// saves when a viewer picks German. It signs in to the Jellyfin at $BASE as $USERNAME, opens the page, lists the language
// select's options, picks the one named German and presses Save, then reads the saved Configuration with GET /Users/Me.
// Prints a JSON object on stdout. Run from a directory with playwright-core.
import { createRequire } from "node:module";
const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");

const { BASE: base, PASSWORD: password, USERNAME: username } = process.env;
const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH, args: ["--disable-gpu"] });
const page = await (await browser.newContext({ viewport: { width: 1280, height: 900 } })).newPage();
try {
  await page.goto(`${base}/web/`);
  await page.waitForSelector("#txtManualName", { timeout: 60000 });
  await page.waitForTimeout(2000);
  await page.fill("#txtManualName", username);
  await page.fill("#txtManualPassword", password);
  await page.click("button[type=submit]");
  await page.waitForFunction(() => !/login|wizard/.test(location.hash) && !!window.ApiClient?.accessToken(), null, { timeout: 30000 });
  const userId = await page.evaluate(() => window.ApiClient.getCurrentUserId());
  await page.goto("about:blank");
  await page.goto(`${base}/web/index.html#/mypreferencessubtitles?userId=${userId}`);
  await page.waitForSelector("#selectSubtitleLanguage", { timeout: 30000 });
  await page.waitForTimeout(1000);
  const options = await page.evaluate(() => [...document.querySelector("#selectSubtitleLanguage").options].map((o) => ({ value: o.value, label: o.textContent })));
  const german = options.filter((o) => o.label === "German");
  if (german.length !== 1) throw new Error(`German options: ${JSON.stringify(german)}`);
  await page.selectOption("#selectSubtitleLanguage", german[0].value);
  await page.click(".btnSave, button[type=submit]");
  await page.waitForTimeout(3000);
  const saved = await page.evaluate(async () => {
    const response = await fetch(`${window.ApiClient.serverAddress()}/Users/Me`, { headers: { Authorization: `MediaBrowser Token="${window.ApiClient.accessToken()}"` } });
    const configuration = (await response.json()).Configuration;
    return { SubtitleLanguagePreference: configuration.SubtitleLanguagePreference, SubtitleMode: configuration.SubtitleMode };
  });
  console.log(JSON.stringify({ option: german[0], saved, germanOptionCount: german.length, optionCount: options.length }));
} finally {
  await browser.close();
}
