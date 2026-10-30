// Drives jellyfin-web in Chromium against a real server and logs its API traffic.
// Usage: node capture.mjs <base_url> <user> <password> <out.json> <movie_search_term>
import { writeFileSync } from "node:fs";
import { createRequire } from "node:module";

const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");

const [base, user, password, outPath, movie] = process.argv.slice(2);
const browser = await chromium.launch({
  executablePath: "/usr/bin/chromium",
  args: ["--autoplay-policy=no-user-gesture-required", "--disable-gpu"],
});
const page = await browser.newPage({ viewport: { width: 1280, height: 720 } });

let stage = "login";
const calls = [];
page.on("request", (request) => {
  const url = new URL(request.url());
  if (!url.href.startsWith(base) || url.pathname.startsWith("/web/")) return;
  if (url.searchParams.get("SearchTerm") === movie) return;
  calls.push({
    stage,
    method: request.method(),
    path: url.pathname,
    query: [...url.searchParams.keys()],
    resource: request.resourceType(),
  });
});
page.on("websocket", (ws) => calls.push({ stage, method: "WS", path: new URL(ws.url()).pathname, query: [] }));

await page.goto(`${base}/web/`, { waitUntil: "domcontentloaded" });
await page.waitForSelector("#txtManualName", { timeout: 30000 });
await page.fill("#txtManualName", user);
await page.fill("#txtManualPassword", password);
await page.click("button[type=submit]");
await page.waitForSelector(".card", { timeout: 30000 });

stage = "browse";
await page.waitForTimeout(3000);
const hash = await page.evaluate(async (name) => {
  const client = window.ApiClient;
  // A plain fetch, so this script's own lookup can't appear as a client call.
  const response = await window.fetch(`${client.serverAddress()}/Items?Recursive=true&IncludeItemTypes=Movie&SearchTerm=${name}`, {
    headers: { Authorization: `MediaBrowser Token="${client.accessToken()}"` },
  });
  const items = await response.json();
  return `#/details?id=${items.Items[0].Id}&serverId=${client.serverId()}`;
}, movie);
await page.goto(`${base}/web/${hash}`);
await page.waitForSelector(".btnPlay", { timeout: 30000 });
await page.waitForTimeout(2000);

stage = "play";
await page.locator(".mainDetailButtons .btnPlay").first().click();
await page.waitForSelector("video", { timeout: 30000 });
await page.waitForFunction(() => document.querySelector("video")?.currentTime > 3, null, { timeout: 60000 });

stage = "seek";
await page.evaluate(() => {
  document.querySelector("video").currentTime = 60;
});
await page.waitForTimeout(6000);
const state = await page.evaluate(() => {
  const video = document.querySelector("video");
  return { currentTime: video.currentTime, src: video.src.slice(0, 90), error: video.error?.message ?? null };
});

stage = "stop";
await page.goBack();
await page.waitForTimeout(3000);
await browser.close();

writeFileSync(outPath, JSON.stringify({ state, calls }, null, 1));
console.log(JSON.stringify(state));
console.log(`${calls.length} calls`);
