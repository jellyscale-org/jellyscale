// Part of capture_audio_menu.py (JS-147): what the real jellyfin-web 12.4 shows in a film's details page Audio menu, and
// what pressing Play does after a pick. It signs in to the Jellyfin at $BASE as $USERNAME, saves each setting of $SETTINGS
// (over $CONFIG), opens each film of $IDS and reads the menu; then, for the picks below, presses Play and records the
// PlaybackInfo it sent and was answered, the tone heard, the player's own audio menu and the details page on return.
// Prints a JSON object on stdout. Run from a directory with playwright-core.
import { createRequire } from "node:module";
const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");

const { BASE: base, PASSWORD: password, ADMIN_TOKEN: admin } = process.env;
const ids = JSON.parse(process.env.IDS);
const settings = JSON.parse(process.env.SETTINGS);
const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH, args: ["--disable-gpu", "--autoplay-policy=no-user-gesture-required"] });

// Each case gets a user of its own: Jellyfin remembers the track a viewer last played in a film, so a pick would otherwise
// reach the next case. The user has the case's two settings saved, as jellyfin-web saves them.
let made = 0;
async function newViewer(language, play) {
  const call = (method, path, body, token) => fetch(`${base}${path}`, { method, body: body && JSON.stringify(body), headers: { "Content-Type": "application/json", Authorization: `MediaBrowser Client="capture", Device="capture", DeviceId="capture", Version="1"${token ? `, Token="${token}"` : ""}` } });
  const username = `viewer${++made}`;
  if ((await call("POST", "/Users/New", { Name: username, Password: password }, admin)).status !== 200) throw new Error("making a user");
  const signed = await (await call("POST", "/Users/AuthenticateByName", { Username: username, Pw: password })).json();
  const body = { ...signed.User.Configuration, AudioLanguagePreference: language, PlayDefaultAudioTrack: play };
  if (language === "") delete body.AudioLanguagePreference;
  const saved = await call("POST", `/Users/${signed.User.Id}/Configuration`, body, signed.AccessToken);
  if (saved.status !== 204) throw new Error(`saving the settings: ${saved.status}`);
  return username;
}

async function signedIn(username) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await context.newPage();
  const infos = [];
  const errors = [];
  page.on("console", (m) => { if (m.type() === "error") errors.push(m.text()); });
  page.on("response", async (r) => {
    if (!/\/Items\/[^/]+\/PlaybackInfo/i.test(r.url()) || r.request().method() !== "POST") return;
    const request = JSON.parse(r.request().postData() ?? "{}");
    const reply = await r.json().catch(() => ({}));
    const source = reply.MediaSources?.[0] ?? {};
    const url = new URL(source.TranscodingUrl ?? "http://none/", base);
    infos.push({
      AudioStreamIndex: request.AudioStreamIndex ?? "<absent>", DefaultAudioStreamIndex: source.DefaultAudioStreamIndex ?? "<absent>",
      SupportsDirectPlay: source.SupportsDirectPlay, SupportsDirectStream: source.SupportsDirectStream,
      TranscodingUrl: source.TranscodingUrl ? { AudioStreamIndex: url.searchParams.get("AudioStreamIndex"), VideoCodec: url.searchParams.get("VideoCodec"), TranscodeReasons: url.searchParams.get("TranscodeReasons") } : null,
    });
  });
  await page.goto(`${base}/web/`);
  await page.waitForSelector("#txtManualName", { timeout: 60000 });
  await page.waitForTimeout(2000); // The page redraws its form once as the client finishes starting.
  await page.fill("#txtManualName", username);
  await page.fill("#txtManualPassword", password);
  await page.click("button[type=submit]");
  await page.waitForFunction(() => !/login|wizard/.test(location.hash) && !!window.ApiClient?.accessToken(), null, { timeout: 30000 }).catch(async (e) => {
    throw new Error(`not signed in: at ${page.url()}, ${await page.evaluate(() => document.body.innerText.slice(0, 300))}; ${errors.join("; ").slice(0, 300)}`);
  });
  return { context, page, infos, errors };
}

async function openDetails(page, film) {
  await page.goto("about:blank"); // A full load: a hash change alone leaves the last film's page in place.
  await page.goto(`${base}/web/index.html#/details?id=${ids[film]}`);
  await page.waitForSelector(".btnPlay:visible", { timeout: 30000 });
  await page.waitForTimeout(1000);
  return readMenu(page);
}

const readMenu = (page) => page.evaluate(() => {
  const select = document.querySelector(".selectAudio");
  const container = document.querySelector(".selectAudioContainer");
  return {
    containerHidden: container.classList.contains("hide"), disabled: select.disabled,
    options: [...select.options].map((o) => ({ value: o.value, label: o.textContent })), selected: select.value,
  };
});

const audioTone = (page) => page.evaluate(async () => {
  const video = document.querySelector("video");
  const context = new AudioContext();
  await context.resume();
  const analyser = context.createAnalyser();
  analyser.fftSize = 16384;
  context.createMediaElementSource(video).connect(analyser);
  analyser.connect(context.destination);
  let strongest = 0;
  for (let round = 0; round < 15 && !strongest; round++) {
    await new Promise((resolve) => setTimeout(resolve, 400));
    const bins = new Float32Array(analyser.frequencyBinCount);
    analyser.getFloatFrequencyData(bins);
    let top = 0;
    for (let i = 1; i < bins.length; i++) if (bins[i] > bins[top]) top = i;
    if (bins[top] > -60) strongest = Math.round((top * context.sampleRate) / analyser.fftSize);
  }
  return strongest;
});

const playing = (page) => page.evaluate(() => document.querySelector("video")?.currentTime ?? 0);

async function playerMenu(page) {
  for (let i = 0; i < 40; i++) {
    await page.mouse.move(400 + Math.random() * 300, 300 + Math.random() * 200);
    if (await page.click(".btnAudio", { timeout: 1500 }).then(() => true, () => false)) break;
  }
  await page.waitForSelector(".actionSheetMenuItem", { timeout: 10000 });
  const entries = await page.evaluate(() => [...document.querySelectorAll(".actionSheetMenuItem")].map((e) => ({ id: e.dataset.id, label: e.innerText.trim().split("\n")[0], selected: e.querySelector(".check")?.style.visibility !== "hidden" })));
  for (let i = 0; i < 10 && (await page.locator(".actionSheetMenuItem").count()); i++) { await page.mouse.click(5, 5); await page.waitForTimeout(400); }
  return entries;
}

const web = { menus: [], picks: [] };
for (const [language, play] of settings) {
  const { context, page } = await signedIn(await newViewer(language, play));
  for (const film of Object.keys(ids)) {
    web.menus.push({ film, AudioLanguagePreference: language, PlayDefaultAudioTrack: play, ...(await openDetails(page, film)) });
  }
  await context.close();
}

// A pick that is neither the menu's selected entry nor, for the German viewer, always the file's default: track 3 of Babel
// for the German viewer (selected 2, file default 1) and for the viewer with no language (selected 1), and the file's
// default 1 for the German viewer.
for (const [film, language, play, pick] of [["Menu Babel", "deu", false, "3"], ["Menu Babel", "deu", false, "1"], ["Menu Babel", "", true, "3"]]) {
  const username = await newViewer(language, play);
  const { context, page, infos, errors } = await signedIn(username);
  const before = await openDetails(page, film);
  await page.selectOption(".selectAudio", pick);
  await page.locator(".btnPlay:visible").first().click();
  const row = { user: username, film, AudioLanguagePreference: language, PlayDefaultAudioTrack: play, selectedBefore: before.selected, picked: pick };
  try {
    for (let i = 0; i < 120 && (await playing(page)) < 3; i++) await page.waitForTimeout(500);
    row.reachedSeconds = Math.round(await playing(page));
    row.tone = await audioTone(page);
    row.playerMenu = await playerMenu(page);
    row.infos = infos;
    const stopped = page.waitForResponse((r) => /\/Sessions\/Playing\/Stopped/i.test(r.url()), { timeout: 15000 }).catch(() => null);
    await page.evaluate(() => window.history.back());
    await stopped;
    await page.waitForSelector(".btnPlay:visible", { timeout: 30000 });
    await page.waitForTimeout(1500);
    row.onReturn = await readMenu(page);
    row.afterReload = await openDetails(page, film); // Seconds later, the page loaded again.
    await context.close();
    // And a viewer who signs in again, in a browser with nothing stored.
    const again = await signedIn(username);
    row.afterSignIn = await openDetails(again.page, film);
    await again.context.close();
  } catch (e) {
    row.failed = String(e);
    row.infos = infos;
  }
  row.errors = errors.filter((e) => !/ActionSheet closed without resolving|^$/.test(e));
  web.picks.push(row);
  await context.close().catch(() => {});
}
await browser.close();
console.log(JSON.stringify(web));
