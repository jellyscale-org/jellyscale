// Phase 1 gate: unmodified jellyfin-web logs in to jellyscale and browses.
// Usage: node browse.mjs <base_url> <user> <password>
// Run from a directory with playwright-core installed.
import { createRequire } from "node:module";
import { tmpdir } from "node:os";

const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");

const [base, user, password] = process.argv.slice(2);
const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH ?? "/usr/bin/chromium", args: ["--disable-gpu"] });
const page = await browser.newPage({ viewport: { width: 1280, height: 720 } });

const failures = [];
const notFound = new Map();
page.on("pageerror", (error) => failures.push(`page error: ${error.message}`));
page.on("response", (response) => {
  const url = new URL(response.url());
  if (!url.href.startsWith(base) || url.pathname.startsWith("/web/")) return;
  if (response.status() >= 500) failures.push(`${response.status()} ${url.pathname}`);
  if (response.status() === 404) {
    const key = url.pathname.replace(/[0-9a-f]{32}/g, "{id}");
    notFound.set(key, (notFound.get(key) ?? 0) + 1);
  }
});

async function step(name, action) {
  try {
    await action();
    console.log(`ok    ${name}`);
  } catch (error) {
    failures.push(`${name}: ${error.message.split("\n")[0]}`);
    console.log(`FAIL  ${name}`);
    await page.screenshot({ path: `${tmpdir()}/jellyscale-e2e-failure.png` }).catch(() => {});
  }
}

const hasText = (text) => page.locator(`:visible:text("${text}")`).first().waitFor({ timeout: 15000 });
// The middle of a card is the play button, so navigate through its title link.
const openCard = async (text) => {
  await page.locator(".cardText a:visible", { hasText: text }).first().click();
};
const hasCard = (text) => page.locator(".card:visible", { hasText: text }).first().waitFor({ timeout: 15000 });

await step("login", async () => {
  await page.goto(`${base}/web/`, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("#txtManualName", { timeout: 30000 });
  await page.fill("#txtManualName", user);
  await page.fill("#txtManualPassword", password);
  await page.click("button[type=submit]");
  await page.waitForSelector(".card", { timeout: 30000 });
});

await step("home shows both libraries", async () => {
  await hasCard("Movies");
  await hasCard("Shows");
});

await step("latest shows scanned movies", async () => {
  await page.waitForFunction(
    () => [...document.querySelectorAll(".cardText")].some((e) => e.textContent.includes("Alpha Film")),
    null, { timeout: 15000 });
});

await step("movie library lists every film in sort order", async () => {
  const listing = page.waitForResponse((response) => /\/Items\?.*recursive=true/i.test(response.url()));
  await page.locator(".card:visible", { hasText: "Movies" }).first().click();
  await listing;
  await hasCard("Gamma Film");
  await page.waitForFunction(() => document.querySelectorAll(".cardText-first:not(:empty)").length >= 3);
  const titles = await page.locator(".itemsContainer:visible .cardText-first").allTextContents();
  if (titles.join() !== "Alpha Film,The Beta Film,Gamma Film") throw new Error(`order: ${titles}`);
});

await step("movie details open with an image", async () => {
  await openCard("Alpha Film");
  await page.waitForSelector(".btnPlay", { timeout: 15000 });
  await hasText("2021");
});

await step("show drills down to an episode", async () => {
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: "Shows" }).first().click();
  await openCard("Delta Show");
  await hasCard("Season 1");
  await hasCard("Season 2");
  await openCard("Season 1");
  await hasText("Pilot");
  await hasText("Second");
});

await step("images load", async () => {
  const response = await page.evaluate(async () => {
    const client = window.ApiClient;
    const headers = { Authorization: `MediaBrowser Token="${client.accessToken()}"` };
    const list = await (await fetch(`${client.serverAddress()}/Items?Recursive=true&IncludeItemTypes=Movie`, { headers })).json();
    const image = await fetch(`${client.serverAddress()}/Items/${list.Items[0].Id}/Images/Primary?maxHeight=300`);
    return { status: image.status, type: image.headers.get("content-type"), size: (await image.blob()).size };
  });
  if (response.status !== 200 || response.size < 1000) throw new Error(JSON.stringify(response));
});

await browser.close();
console.log("\nunknown routes:", notFound.size ? [...notFound].map(([k, v]) => `${k} x${v}`).join(", ") : "none");
if (failures.length) {
  console.log("\nFAILED");
  for (const failure of failures) console.log(`  - ${failure}`);
  process.exit(1);
}
console.log("\nPASS");
