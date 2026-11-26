// Phase 1 gate: unmodified jellyfin-web logs in to jellyscale and browses.
// Usage: node browse.mjs <base_url> <user> <password>
// Run from a directory with playwright-core installed.
import { createRequire } from "node:module";
import { wholeLine } from "./shard.mjs";
import { recordResponses } from "./conform.mjs";
import { configure, login } from "./player.mjs";
import { keepFailure, watch } from "./evidence.mjs";

const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");

const [base, user, password] = process.argv.slice(2);
configure({ base, user, password });
const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH ?? "/usr/bin/chromium", args: ["--disable-gpu"] });
const page = await browser.newPage({ viewport: { width: 1280, height: 720 } });
watch(page.context(), base);
recordResponses(page, base);

const failures = [];
let pendingEvidence = Promise.resolve();
let currentStep = "before the first step";
let currentStepBegan = 0;
// Names are free until the console file is written, so simultaneous page events must not choose the same one.
const queueEvidence = (name, began) => {
  pendingEvidence = pendingEvidence.then(() => keepFailure(name, began));
  return pendingEvidence;
};
const collectFailure = (kind, message) => {
  failures.push(message);
  queueEvidence(`${currentStep}: ${kind}`, currentStepBegan);
};
const notFound = new Map();
page.on("pageerror", (error) => collectFailure("page error", `page error: ${error.message}`));
page.on("response", (response) => {
  const url = new URL(response.url());
  if (!url.href.startsWith(base) || url.pathname.startsWith("/web/")) return;
  if (response.status() >= 500) collectFailure("server error", `${response.status()} ${url.pathname}`);
  if (response.status() === 404) {
    const key = url.pathname.replace(/[0-9a-f]{32}/g, "{id}");
    notFound.set(key, (notFound.get(key) ?? 0) + 1);
  }
});

let steps = 0;
async function step(name, action) {
  steps++;
  const began = Date.now();
  currentStep = name;
  currentStepBegan = began;
  try {
    await action();
    console.log(`ok    ${name} (${((Date.now() - began) / 1000).toFixed(1)}s)`);
  } catch (error) {
    failures.push(`${name}: ${error.message.split("\n")[0]}`);
    console.log(`FAIL  ${name} (${((Date.now() - began) / 1000).toFixed(1)}s)`);
    await queueEvidence(name, began);
  }
}

const hasText = (text) => page.locator(`:visible:text("${text}")`).first().waitFor({ timeout: 15000 });
// The middle of a card is the play button, so navigate through its title link.
const openCard = async (text) => {
  await page.locator(".cardText a:visible", { hasText: text }).first().click();
};
const hasCard = (text) => page.locator(".card:visible", { hasText: text }).first().waitFor({ timeout: 15000 });

await step("login", () => login(page));

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

// JS-116: jellyfin-web saves a viewer's settings by posting the whole Configuration to POST /Users/{id}/Configuration
// and fills its language lists from GET /Localization/Cultures (what a real Jellyfin 12.4 shows is in
// compat/captures/user-configuration-12.4.md). The admin's own settings are put back at the end: they act on no
// track yet, but the gates after this one share the user.
const me = () => page.evaluate(async () => {
  const client = window.ApiClient;
  const reply = await fetch(`${client.serverAddress()}/Users/Me`, { headers: { Authorization: `MediaBrowser Token="${client.accessToken()}"` } });
  return { status: reply.status, user: await reply.json() };
});
const settingsPage = async (name, select) => {
  await page.goto(`${base}/web/#/${name}`, { waitUntil: "domcontentloaded" });
  await page.waitForFunction((id) => document.getElementById(id)?.options.length > 1, select, { timeout: 30000 });
};
const languages = (select) => page.evaluate((id) => [...document.getElementById(id).options].map((o) => ({ value: o.value, text: o.text })), select);
// Saves the page, which posts the whole Configuration, and says what the server answered.
const savePage = async () => {
  const [reply] = await Promise.all([
    page.waitForResponse((r) => r.request().method() === "POST" && /\/Users\/[^/]+\/Configuration/i.test(r.url()), { timeout: 15000 }),
    page.click("button.btnSave:visible"),
  ]);
  if (!reply.ok()) throw new Error(`saving answered ${reply.status()}`);
  await page.getByText("Settings saved").first().waitFor({ timeout: 10000 });
};
let originalConfiguration;

await step("Subtitles settings: German and Always are saved and survive a reload", async () => {
  const before = await me();
  originalConfiguration = before.user.Configuration;
  await settingsPage("mypreferencessubtitles", "selectSubtitleLanguage");
  const options = await languages("selectSubtitleLanguage");
  const german = options.find((o) => o.value === "deu");
  if (options.length < 100 || !german || german.text !== "German" || options.some((o) => o.value && !o.text.trim())) {
    throw new Error(`the language list holds ${options.length} languages, German is ${JSON.stringify(german)}`);
  }
  await page.selectOption("#selectSubtitleLanguage", "deu");
  await page.selectOption("#selectSubtitlePlaybackMode", "Always");
  await savePage();
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.waitForFunction(() => document.getElementById("selectSubtitleLanguage")?.options.length > 1, null, { timeout: 30000 });
  const shown = await page.evaluate(() => ({ language: document.getElementById("selectSubtitleLanguage").value, mode: document.getElementById("selectSubtitlePlaybackMode").value }));
  if (shown.language !== "deu" || shown.mode !== "Always") throw new Error(`after a reload the page shows ${JSON.stringify(shown)}`);
  const { status, user } = await me();
  const config = user.Configuration;
  if (status !== 200 || config.SubtitleLanguagePreference !== "deu" || config.SubtitleMode !== "Always") {
    throw new Error(`GET /Users/Me holds ${config?.SubtitleLanguagePreference} and ${config?.SubtitleMode}`);
  }
});

await step("Playback settings: German audio and no default track are saved, and the Subtitles page keeps its own", async () => {
  await settingsPage("mypreferencesplayback", "selectAudioLanguage");
  const options = await languages("selectAudioLanguage");
  if (options.length < 100 || !options.some((o) => o.value === "deu" && o.text === "German") || !options.some((o) => o.value === "OriginalLanguage")) {
    throw new Error(`the audio language list holds ${options.length} entries`);
  }
  await page.selectOption("#selectAudioLanguage", "deu");
  if (await page.locator(".chkPlayDefaultAudioTrack").isChecked()) {
    await page.locator("label", { hasText: "Play default audio track regardless of language" }).click();
  }
  await savePage();
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.waitForFunction(() => document.getElementById("selectAudioLanguage")?.options.length > 1, null, { timeout: 30000 });
  const shown = await page.evaluate(() => ({ language: document.getElementById("selectAudioLanguage").value, playDefault: document.querySelector(".chkPlayDefaultAudioTrack").checked }));
  if (shown.language !== "deu" || shown.playDefault !== false) throw new Error(`after a reload the Playback page shows ${JSON.stringify(shown)}`);
  await settingsPage("mypreferencessubtitles", "selectSubtitleLanguage");
  const subtitles = await page.evaluate(() => ({ language: document.getElementById("selectSubtitleLanguage").value, mode: document.getElementById("selectSubtitlePlaybackMode").value }));
  if (subtitles.language !== "deu" || subtitles.mode !== "Always") throw new Error(`the Subtitles page shows ${JSON.stringify(subtitles)} after the Playback page saved`);
  const config = (await me()).user.Configuration;
  if (config.AudioLanguagePreference !== "deu" || config.PlayDefaultAudioTrack !== false || config.SubtitleLanguagePreference !== "deu" || config.SubtitleMode !== "Always") {
    throw new Error(`GET /Users/Me holds ${JSON.stringify(config)}`);
  }
});

if (originalConfiguration) {
  await step("the admin's settings are put back", async () => {
    const status = await page.evaluate(async (config) => {
      const client = window.ApiClient;
      const reply = await fetch(`${client.serverAddress()}/Users/Configuration`, {
        method: "POST", headers: { "Content-Type": "application/json", Authorization: `MediaBrowser Token="${client.accessToken()}"` }, body: JSON.stringify(config) });
      return reply.status;
    }, originalConfiguration);
    if (status !== 204) throw new Error(`putting the settings back answered ${status}`);
  });
}

// Shows E2E_REPEAT reporting a scenario that passes only some of the time: this
// fails on odd iterations. It runs only when E2E_FLAKY_DEMO is set, never in CI.
if (process.env.E2E_FLAKY_DEMO) {
  await step("flaky demo: fails on odd iterations", async () => {
    if (Number(process.env.E2E_ITERATION) % 2 === 1) throw new Error("odd iteration");
  });
}

await pendingEvidence;
await browser.close();
console.log("\nunknown routes:", notFound.size ? [...notFound].map(([k, v]) => `${k} x${v}`).join(", ") : "none");
if (failures.length) {
  console.log("\nFAILED");
  for (const failure of failures) console.log(`  - ${failure}`);
  wholeLine(steps);
  process.exit(1);
}
console.log("\nPASS");
wholeLine(steps);
