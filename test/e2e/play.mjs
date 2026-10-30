// Phase 2 gate: unmodified jellyfin-web direct-plays a POSIX film and an S3
// film through the router, seeks, stops partway, and a fresh login finds the
// position saved and the film under Continue Watching.
// Usage: node play.mjs <base_url> <user> <password>
// Run from a directory with playwright-core installed.
import { createRequire } from "node:module";

const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");

const [base, user, password] = process.argv.slice(2);
const films = [
  { library: "Movies", title: "Alpha Film", backend: "POSIX" },
  { library: "Cloud", title: "Epsilon Film", backend: "S3" },
];

const browser = await chromium.launch({
  executablePath: "/usr/bin/chromium",
  args: ["--disable-gpu", "--autoplay-policy=no-user-gesture-required"],
});
const failures = [];

async function newSession() {
  const context = await browser.newContext({ viewport: { width: 1280, height: 720 } });
  const page = await context.newPage();
  const seen = { videos: [], errors: [], sockets: [], frames: [] };
  page.on("pageerror", (error) => seen.errors.push(error.message));
  page.on("response", (response) => {
    const url = new URL(response.url());
    if (!response.url().startsWith(base)) return;
    if (response.status() >= 500) seen.errors.push(`${response.status()} ${url.pathname}`);
    if (/^\/videos\//i.test(url.pathname)) {
      seen.videos.push({ status: response.status(), routed: response.headers()["x-routed-to"], path: url.pathname });
    }
  });
  page.on("websocket", (socket) => {
    seen.sockets.push(socket.url());
    socket.on("framereceived", (frame) => seen.frames.push(String(frame.payload)));
  });
  return { context, page, seen };
}

async function login(page) {
  await page.goto(`${base}/web/`, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("#txtManualName", { timeout: 30000 });
  await page.fill("#txtManualName", user);
  await page.fill("#txtManualPassword", password);
  await page.click("button[type=submit]");
  await page.waitForSelector(".card", { timeout: 30000 });
}

async function step(name, action) {
  try {
    await action();
    console.log(`ok    ${name}`);
  } catch (error) {
    failures.push(`${name}: ${error.message.split("\n")[0]}`);
    console.log(`FAIL  ${name}: ${error.message.split("\n")[0]}`);
  }
}

const time = (page) => page.evaluate(() => document.querySelector("video")?.currentTime ?? -1);
const duration = (page) => page.evaluate(() => document.querySelector("video")?.duration ?? 0);

async function waitFor(page, what, test, timeout = 20000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const value = await test();
    if (value) return value;
    await page.waitForTimeout(200);
  }
  throw new Error(`timed out waiting for ${what}`);
}

const positions = {};
const first = await newSession();
await step("login", () => login(first.page));

for (const film of films) {
  const { page, seen } = first;
  await step(`${film.backend}: play ${film.title}`, async () => {
    await page.goto(`${base}/web/`);
    await page.waitForSelector(".card", { timeout: 30000 });
    await page.locator(".card:visible", { hasText: film.library }).first().click();
    await page.locator(".cardText a:visible", { hasText: film.title }).first().click();
    await page.locator(".btnPlay:visible").first().click();
    await waitFor(page, "video to start", async () => (await time(page)) > 0.5);
  });

  let middle = 0;
  await step(`${film.backend}: time advances`, async () => {
    const before = await time(page);
    await waitFor(page, "currentTime to advance", async () => (await time(page)) > before + 1);
  });
  await step(`${film.backend}: seek to the middle and keep playing`, async () => {
    const total = await duration(page);
    if (!(total > 5)) throw new Error(`duration ${total}`);
    middle = total * 0.45;
    await page.evaluate((target) => { document.querySelector("video").currentTime = target; }, middle);
    await waitFor(page, "playback past the middle", async () => (await time(page)) > middle + 1);
    const now = await time(page);
    if (now > total * 0.6) throw new Error(`overshot to ${now} of ${total}`);
  });
  await step(`${film.backend}: direct play came from the stream replica`, async () => {
    const stream = seen.videos.filter((v) => /\/stream/i.test(v.path));
    if (!stream.length) throw new Error("no /Videos/*/stream request");
    const bad = stream.filter((v) => v.routed !== "stream" || ![200, 206].includes(v.status));
    if (bad.length) throw new Error(JSON.stringify(bad));
    if (!stream.some((v) => v.status === 206)) throw new Error("no ranged response");
  });
  await step(`${film.backend}: stop reports the position`, async () => {
    const stopped = page.waitForResponse((r) => /\/Sessions\/Playing\/Stopped/i.test(r.url()), { timeout: 15000 });
    const total = await duration(page);
    const stoppedAt = await time(page);
    // Leaving the player stops playback and sends the report.
    await page.evaluate(() => window.history.back());
    const response = await stopped;
    if (response.status() !== 204) throw new Error(`stopped answered ${response.status()}`);
    positions[film.title] = { total, stoppedAt };
    const fraction = stoppedAt / total;
    if (fraction < 0.4 || fraction > 0.6) throw new Error(`stopped at ${(fraction * 100).toFixed(0)}%`);
  });
  if (seen.errors.length) failures.push(`${film.backend}: ${seen.errors.join("; ")}`);
}
await step("web socket got ForceKeepAlive through the router", async () => {
  const { sockets, frames } = first.seen;
  if (!sockets.length) throw new Error("the client never opened /socket");
  if (!frames.some((f) => /"MessageType":"ForceKeepAlive"/.test(f) && /"Data":60/.test(f))) throw new Error(`frames: ${frames.slice(0, 3)}`);
});
await first.context.close();

// A new browser has no memory of the first: everything must come from the server.
const second = await newSession();
await step("fresh login", () => login(second.page));
await step("positions persisted and films are under Continue Watching", async () => {
  const { page } = second;
  const resumable = await page.evaluate(async () => {
    const client = window.ApiClient;
    const url = `${client.serverAddress()}/UserItems/Resume?Limit=50&Recursive=true&Fields=MediaSources`;
    const result = await (await fetch(url, { headers: { Authorization: `MediaBrowser Token="${client.accessToken()}"` } })).json();
    return result.Items.map((item) => ({ name: item.Name, ticks: item.UserData.PlaybackPositionTicks, runtime: item.RunTimeTicks }));
  });
  for (const film of films) {
    const item = resumable.find((i) => i.name === film.title);
    if (!item) throw new Error(`${film.title} is not resumable: ${JSON.stringify(resumable)}`);
    const fraction = item.ticks / item.runtime;
    if (fraction < 0.4 || fraction > 0.6) throw new Error(`${film.title} saved at ${(fraction * 100).toFixed(0)}%`);
  }
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  for (const film of films) {
    await waitFor(page, `${film.title} under Continue Watching`, () => page.evaluate((title) => {
      const heading = [...document.querySelectorAll(".sectionTitle")].find((e) => /continue watching/i.test(e.textContent));
      const section = heading?.closest(".verticalSection");
      return !!section && [...section.querySelectorAll(".cardText")].some((e) => e.textContent.includes(title));
    }, film.title));
  }
});
await second.context.close();
await browser.close();

if (failures.length) {
  console.log("\nFAILED");
  for (const failure of failures) console.log(`  - ${failure}`);
  process.exit(1);
}
console.log("\nPASS");
