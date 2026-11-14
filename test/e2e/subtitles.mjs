// Phase 3 gate: unmodified jellyfin-web shows text subtitles, from POSIX and S3,
// on direct play and on a transcode, and draws them into the picture when the
// viewer's settings ask for it. The tracks are in the film or in files beside
// it, and ASS is drawn in the fonts attached to the film. PGS tracks are drawn by
// jellyfin-web from the file when its viewer has that on, and into the picture
// by a transcode otherwise, as VobSub always is. Needs the stack test/e2e/run.sh starts.
// Usage: E2E_SCRATCH=<worker scratch dir> E2E_PG=<postgres container> node subtitles.mjs <base_url> <user> <password>
// Run from a directory with playwright-core installed.
import { execFile, execFileSync, spawn } from "node:child_process";
import { promisify } from "node:util";
import { existsSync, readFileSync, readdirSync, rmSync, statSync, truncateSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { createRequire } from "node:module";
import { advances, seek, time, waitFor } from "./player.mjs";
import { runLater, schedule, shardLine } from "./shard.mjs";
import { recordResponses } from "./conform.mjs";
import { everyReplicaReady, postgres, routerControl, withTwoStreamReplicas } from "./away.mjs";

const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");

const [base, user, password] = process.argv.slice(2);
const scratch = process.env.E2E_SCRATCH;
if (!scratch || !process.env.E2E_PG) throw new Error("E2E_SCRATCH and E2E_PG say where the worker and the database are");

const browser = await chromium.launch({
  executablePath: process.env.CHROMIUM_PATH ?? "/usr/bin/chromium",
  args: ["--disable-gpu", "--autoplay-policy=no-user-gesture-required"],
});
const failures = [];

const exec_ = async (cmd, args) => (await promisify(execFile)(cmd, args)).stdout;
const sql = (text) => execFileSync("docker", ["exec", process.env.E2E_PG, "psql", "-U", "postgres", "-Atc", text]).toString().trim();
// Specs with a job, not jobs: a seek past what the job has made (transcodeLead)
// starts another job of the same spec, whereas picking a track that is drawn into
// the picture would be another spec.
const jobCount = () => Number(sql("SELECT count(DISTINCT spec_hash) FROM transcode_jobs"));
const subtitleJobCount = () => Number(sql("SELECT count(*) FROM subtitle_jobs"));

// The worker's ffmpeg processes, read from the process table.
function ffmpegRuns() {
  try {
    return execFileSync("pgrep", ["-af", `ffmpeg.*${scratch}`]).toString().trim().split("\n").filter(Boolean)
      .map((line) => ({ line, start: Number(/-start_number (\d+)/.exec(line)?.[1]), burns: /subtitles=subtitles\.ass|overlay=eof_action/.test(line) }));
  } catch {
    return []; // pgrep exits 1 when nothing matches.
  }
}

// Every ffmpeg command line the worker has run, from the shim's log beside the
// server's.
const ffmpegLines = () => readFileSync(join(dirname(process.env.E2E_LOG), "ffmpeg.log"), "utf8").split("\n").filter(Boolean);

async function login(page) {
  await page.goto(`${base}/web/`, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("#txtManualName", { timeout: 30000 });
  await page.fill("#txtManualName", user);
  await page.fill("#txtManualPassword", password);
  await page.click("button[type=submit]");
  await page.waitForSelector(".card", { timeout: 30000 });
}

// A signed-in browser that records what the server told it.
async function viewer() {
  const context = await browser.newContext({ viewport: { width: 1280, height: 720 } });
  const page = await context.newPage();
  recordResponses(page, base);
  const seen = { infos: [], playlists: [], subtitles: [], attachments: [], configs: 0, deletes: [], errors: [], answers: [] };
  page.on("pageerror", (error) => seen.errors.push(error.message));
  page.on("request", (request) => {
    if (request.method() === "DELETE" && /ActiveEncodings/i.test(request.url())) seen.deletes.push(request.url());
  });
  page.on("response", async (response) => {
    if (!response.url().startsWith(base)) return;
    const url = new URL(response.url());
    seen.answers.push({ at: Date.now(), status: response.status(), url: response.url() });
    if (response.status() >= 500) seen.errors.push(`${response.status()} ${url.pathname}`);
    if (/\.m3u8$/i.test(url.pathname)) seen.playlists.push(url.pathname + url.search);
    if (/\/Subtitles\//i.test(url.pathname)) {
      seen.subtitles.push({ at: Date.now(), status: response.status(), type: response.headers()["content-type"], path: url.pathname, index: Number(url.pathname.split("/")[5]) });
    }
    if (/\/Attachments\/\d+$/i.test(url.pathname)) {
      seen.attachments.push({ status: response.status(), type: response.headers()["content-type"], path: url.pathname, bytes: (await response.body().catch(() => Buffer.alloc(0))).length });
    }
    if (/\/System\/Configuration\/encoding$/i.test(url.pathname) && response.status() === 200) seen.configs++;
    if (response.request().method() === "POST" && /\/PlaybackInfo$/i.test(url.pathname)) {
      const request = JSON.parse(response.request().postData() ?? "{}");
      const reply = await response.json().catch(() => null);
      if (reply) seen.infos.push({ request, source: reply.MediaSources[0], session: reply.PlaySessionId });
    }
  });
  await login(page);
  return { context, page, seen };
}

async function play(page, library, title) {
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: library }).first().click();
  await page.locator(".cardText a:visible", { hasText: title }).first().click();
  await page.locator(".btnPlay:visible").first().click();
  await waitFor("video to start", async () => (await time(page)) > 0.5, 60000);
}

function noErrors(seen) {
  if (seen.errors.length) throw new Error(`errors: ${seen.errors.join("; ")}`);
  const bad = seen.subtitles.filter((r) => r.status !== 200 && r.status !== 206); // 206: jellyfin-web probes a PGS file for ranges.
  if (bad.length) throw new Error(`subtitle requests failed: ${JSON.stringify(bad.slice(0, 3))}`);
}

// E2E_ONLY picks scenarios by name and E2E_SHARD by call order (see shard.mjs).
function scenario(name, run, options = {}) {
  return schedule(name, options, async () => {
    const began = Date.now();
    const v = await viewer();
    let passed = false;
    try {
      await run(v);
      passed = true;
    } catch (error) {
      const why = error.message.split("\n").filter(Boolean).slice(0, 3).join(" / ");
      failures.push(`${name}: ${why}`);
      console.log(`FAIL  ${name}: ${why} (${((Date.now() - began) / 1000).toFixed(1)}s)`);
      await v.page.screenshot({ path: `${process.env.TMPDIR ?? "/tmp"}/jellyscale-e2e-subtitles.png` }).catch(() => {});
    } finally {
      await v.context.close().catch(() => {});
    }
    await waitFor("ffmpeg to exit between scenarios", async () => ffmpegRuns().length === 0, 90000).catch(() => {
      failures.push(`${name}: ffmpeg still running afterwards`);
      console.log(`FAIL  ${name}: ffmpeg still running afterwards (${((Date.now() - began) / 1000).toFixed(1)}s)`);
      passed = false;
    });
    if (passed) console.log(`ok    ${name} (${((Date.now() - began) / 1000).toFixed(1)}s)`);
  });
}

const leave = async (page) => {
  // The player reports its start without waiting for its page, and only that
  // page sends Stopped on leaving. It listens for leaving before it first shows
  // the position, so going back any sooner leaves the film playing.
  await page.waitForFunction(() => location.hash.startsWith("#/video") && document.querySelector(".osdPositionText")?.textContent, null, { timeout: 15000 });
  const stopped = page.waitForResponse((r) => /\/Sessions\/Playing\/Stopped/i.test(r.url()), { timeout: 15000 });
  await page.evaluate(() => window.history.back());
  await stopped;
};

// Picks a track in the player's subtitle menu: its stream index, or -1 for off.
// The controls hide when the mouse rests and show when it moves somewhere new,
// so each try moves it.
async function pickSubtitle(page, index) {
  await waitFor("the subtitle button", async () => {
    await page.mouse.move(400 + Math.random() * 300, 300 + Math.random() * 200);
    return page.click(".btnSubtitles", { timeout: 1500 }).then(() => true, () => false);
  }, 30000);
  await page.click(`.actionSheetMenuItem[data-id="${index}"]`, { timeout: 10000 });
}

// How many lit pixels the picture has in its top, middle and bottom third, read
// from the paused frame. The burn-in films are black, so only the subtitle
// counts: white text, or with yellow set the yellow bars of an image track.
async function brightThirds(page, yellow = false) {
  await page.evaluate(() => document.querySelector("video").pause());
  await new Promise((resolve) => setTimeout(resolve, 400));
  const thirds = await page.evaluate((yellow) => {
    const canvas = document.createElement("canvas");
    canvas.width = 320;
    canvas.height = 180;
    const context = canvas.getContext("2d", { willReadFrequently: true });
    context.drawImage(document.querySelector("video"), 0, 0, 320, 180);
    const data = context.getImageData(0, 0, 320, 180).data;
    const counts = [0, 0, 0];
    for (let i = 0; i < data.length; i += 4) {
      if (data[i] > 128 && data[i + 1] > 128 && (yellow ? data[i + 2] < 100 : data[i + 2] > 128)) counts[Math.floor(i / 4 / 320 / 60)]++;
    }
    return counts;
  }, yellow);
  await page.evaluate(() => document.querySelector("video").play());
  return thirds;
}

// The same count for what the page shows on screen, the picture and what
// jellyfin-web draws over it, which is how a PGS track the client draws is seen.
async function screenThirds(page) {
  const png = (await page.screenshot()).toString("base64");
  return page.evaluate(async (png) => {
    const image = new Image();
    image.src = `data:image/png;base64,${png}`;
    await image.decode();
    const canvas = document.createElement("canvas");
    canvas.width = 320;
    canvas.height = 180;
    const context = canvas.getContext("2d", { willReadFrequently: true });
    context.drawImage(image, 0, 0, 320, 180);
    const data = context.getImageData(0, 0, 320, 180).data;
    const counts = [0, 0, 0];
    for (let i = 0; i < data.length; i += 4) {
      if (data[i] > 128 && data[i + 1] > 128 && data[i + 2] < 100) counts[Math.floor(i / 4 / 320 / 60)]++;
    }
    return counts;
  }, png);
}

// Where the text should be at a second of the black film: top, middle or
// bottom, or nowhere.
function expectBright(thirds, where) {
  const names = ["top", "middle", "bottom"];
  const found = thirds.map((n, i) => (n >= 20 ? names[i] : null)).filter(Boolean);
  const want = where ? [where] : [];
  if (found.join() !== want.join() || thirds.some((n, i) => names[i] !== where && n > 3)) {
    throw new Error(`bright pixels top ${thirds[0]}, middle ${thirds[1]}, bottom ${thirds[2]}; wanted ${where ?? "none"}`);
  }
}

// The text the page is drawing from the SubRip track now (jellyfin-web puts a
// left-to-right mark in front), and how many pixels the ASS renderer has drawn.
const activeText = (page) => page.evaluate(() => [...document.querySelector("video").textTracks].flatMap((t) => [...(t.activeCues ?? [])].map((c) => c.text.replace(/\u200e/g, ""))).join("|"));
const assPixels = (page) => page.evaluate(() => {
  const canvas = document.querySelector("canvas.libassjs-canvas");
  if (!canvas) return -1;
  const data = canvas.getContext("2d", { willReadFrequently: true }).getImageData(0, 0, canvas.width, canvas.height).data;
  let count = 0;
  for (let i = 3; i < data.length; i += 4) if (data[i] > 0) count++;
  return count;
});

// SubRip and ASS tracks listed, picked and shown, with no new PlaybackInfo, no
// job and no playlist request for the pick.
// With beside set the two tracks are files next to the film (an English SubRip
// and a Japanese ASS file), which a worker converts when the player first asks:
// one subtitle job each, and none for picking them again.
function clientRendered(name, library, title, transcoded, beside = false) {
  return scenario(name, async ({ page, seen }) => {
    const subtitleJobs0 = subtitleJobCount();
    await play(page, library, title);
    const source = seen.infos.at(-1).source;
    if (transcoded !== Boolean(source.TranscodingUrl)) throw new Error(`transcoded should be ${transcoded}: ${source.TranscodingUrl}`);
    const tracks = source.MediaStreams.filter((s) => s.Type === "Subtitle");
    const [srt, ass] = [tracks.find((t) => t.Index === 2), tracks.find((t) => t.Index === 3)];
    if (tracks.length !== 2 || srt?.DeliveryMethod !== "External" || ass?.DeliveryMethod !== "External" || srt.IsExternalUrl !== false ||
      !/^\/Videos\/[0-9a-f-]{36}\/[0-9a-f]{32}\/Subtitles\/2\/0\/Stream\.vtt\?ApiKey=\w+$/i.test(srt.DeliveryUrl) ||
      !/\/Subtitles\/3\/0\/Stream\.ass\?ApiKey=\w+$/i.test(ass.DeliveryUrl)) {
      throw new Error(`tracks: ${JSON.stringify(tracks)}`);
    }
    if (beside && (!srt.IsExternal || srt.Language !== "eng" || srt.DisplayTitle !== "English - SUBRIP - External" || !ass.IsExternal || ass.Language !== "jpn" || ass.DisplayTitle !== "Japanese - ASS - External")) {
      throw new Error(`files beside the film: ${JSON.stringify([srt, ass])}`);
    }
    await advances(page, 3);
    await pickSubtitle(page, -1); // Whatever the client started with, begin from off.
    await new Promise((resolve) => setTimeout(resolve, 1500));
    const infos = seen.infos.length, jobs = jobCount(), playlists = seen.playlists.length;

    await pickSubtitle(page, 2);
    await waitFor("the SubRip track", async () => seen.subtitles.find((r) => r.index === 2 && r.status === 200));
    const js = seen.subtitles.find((r) => r.index === 2);
    if (!/Stream\.js$/i.test(js.path) || !/^application\/json/.test(js.type)) throw new Error(`SubRip was fetched as ${js.path} ${js.type}`);
    await seek(page, 12);
    await waitFor("the first cue", async () => (await activeText(page)) === "English line 1");
    await seek(page, 22);
    await waitFor("the second cue", async () => (await activeText(page)) === "English line 2");

    await pickSubtitle(page, 3);
    await waitFor("the ASS track", async () => seen.subtitles.find((r) => r.index === 3 && r.status === 200));
    const file = seen.subtitles.find((r) => r.index === 3);
    if (!/Stream\.ass$/i.test(file.path) || file.type !== "text/x-ssa" || seen.configs < 1) throw new Error(`ASS: ${file.path} ${file.type}, encoding config fetched ${seen.configs} times`);
    await seek(page, 12);
    await waitFor("ASS text drawn", async () => (await assPixels(page)) > 100);
    await seek(page, 18);
    await waitFor("ASS text gone between cues", async () => (await assPixels(page)) === 0);

    if (beside && subtitleJobCount() - subtitleJobs0 !== 2) throw new Error(`two files should be two subtitle jobs, not ${subtitleJobCount() - subtitleJobs0}`);
    if (seen.infos.length !== infos || jobCount() !== jobs || seen.playlists.length !== playlists) {
      throw new Error(`picking subtitles caused work: ${seen.infos.length - infos} PlaybackInfo, ${jobCount() - jobs} jobs, ${seen.playlists.length - playlists} playlists`);
    }
    if (!transcoded && ffmpegRuns().length) throw new Error("ffmpeg ran for a direct play");
    noErrors(seen);
    await leave(page);
  });
}

await clientRendered("POSIX direct play: SubRip and ASS tracks show, and picking makes no PlaybackInfo, job or playlist", "Transcode", "Sigma Film", false);
await clientRendered("POSIX transcode: SubRip and ASS tracks show, and picking makes no PlaybackInfo, job or playlist", "Transcode", "Tau Film", true);
await clientRendered("S3 direct play: SubRip and ASS tracks show", "Cloud", "Chi Film", false);
await clientRendered("S3 transcode: SubRip and ASS tracks show", "Cloud", "Psi Film", true);

await clientRendered("POSIX direct play, subtitle files beside the film: SubRip and ASS tracks show, and picking makes no PlaybackInfo, job or playlist", "Transcode", "Direct Sub Film", false, true);
await clientRendered("POSIX transcode, subtitle files beside the film: SubRip and ASS tracks show", "Transcode", "Transcode Sub Film", true, true);
await clientRendered("S3 direct play, subtitle files beside the film: SubRip and ASS tracks show", "Cloud", "Beside Direct Film", false, true);
await clientRendered("S3 transcode, subtitle files beside the film: SubRip and ASS tracks show", "Cloud", "Beside Transcode Film", true, true);

// Switching between two text tracks of a film: the first pick has a worker read the
// film once for every text track in it (one ffmpeg, whatever the track), and the
// second pick, and going back to the first, show their text with no new ffmpeg and no
// new subtitle job. The film has no scenario before this one: its tracks are not
// stored until the first pick here. A fonts dump is another job, not a read for tracks.
function switchingTracks(name, library, title) {
  const extractions = () => ffmpegLines().filter((line) => !/-dump_attachment/.test(line)).length;
  const trackJobs = () => Number(sql("SELECT count(*) FROM subtitle_jobs WHERE kind NOT IN ('fonts', 'chapters')"));
  return scenario(name, async ({ page, seen }) => {
    await play(page, library, title);
    await advances(page, 3);
    await pickSubtitle(page, -1);
    await new Promise((resolve) => setTimeout(resolve, 1500));
    const reads0 = extractions(), jobs0 = trackJobs();

    await pickSubtitle(page, 2);
    await waitFor("the SubRip track", async () => seen.subtitles.find((r) => r.index === 2 && r.status === 200));
    await seek(page, 12);
    await waitFor("the first cue", async () => (await activeText(page)) === "English line 1");
    const reads1 = extractions(), jobs1 = trackJobs();
    if (reads1 - reads0 !== 1) throw new Error(`the first pick should be one read of the film (one ffmpeg), not ${reads1 - reads0}`);

    await pickSubtitle(page, 3);
    await waitFor("the ASS track", async () => seen.subtitles.find((r) => r.index === 3 && r.status === 200));
    await seek(page, 12);
    await waitFor("ASS text drawn", async () => (await assPixels(page)) > 100);
    await pickSubtitle(page, 2);
    await seek(page, 22);
    await waitFor("the SubRip track again", async () => (await activeText(page)) === "English line 2");
    if (extractions() !== reads1) throw new Error(`switching tracks read the film again: ${extractions() - reads1} more ffmpeg runs`);
    if (trackJobs() !== jobs1) throw new Error(`switching tracks started ${trackJobs() - jobs1} subtitle jobs, after ${jobs1 - jobs0} for the first pick`);
    noErrors(seen);
    await leave(page);
  });
}
await switchingTracks("POSIX direct play: picking a second text track costs no new read of the film", "Transcode", "Pick Film");
await switchingTracks("S3 direct play: picking a second text track costs no new read of the film", "Cloud", "Pick Cloud Film");

// The ASS track names a font the renderer doesn't have. With the font attached
// to the film, jellyfin-web fetches it from the DeliveryUrl in MediaAttachments
// and the renderer draws the text in it; without, in its fallback, which is
// wider or narrower, so the pixels it covers differ. A film's fonts are listed
// once a worker has stored them, which PlaybackInfo starts, so the first play
// of a film draws in the fallback.
async function drawn(page, seen, title) {
  const asked = seen.attachments.length;
  await play(page, "Transcode", title);
  await pickSubtitle(page, 2);
  await seek(page, 5);
  await waitFor("ASS text drawn", async () => (await assPixels(page)) > 100);
  await new Promise((resolve) => setTimeout(resolve, 500)); // Fonts load after the first draw.
  const pixels = await assPixels(page);
  await leave(page);
  return { pixels, fonts: seen.attachments.slice(asked) };
}
const fontsJobDone = (title) => waitFor(`the fonts job of ${title}`, async () =>
  Number(sql(`SELECT count(*) FROM subtitle_jobs j JOIN items i ON replace(i.id::text, '-', '') = replace(j.item_id, '-', '') WHERE j.kind = 'fonts' AND j.state = 'done' AND i.name = '${title}'`)) === 1);
// The fallback draws the same line to the pixel on every run and host (19058
// here and in CI), but an attached serif may cover only 8% more or less than
// it (DejaVu Serif on Ubuntu), so "the same" has to be close.
const sameText = (a, b) => Math.abs(a - b) < 0.02 * b;
// The fallback's pixels, which these scenarios compare with, measured by whichever
// of them runs first on this shard: they run in any order, on any shard.
let measured;
const fallbackPixels = async (page, seen) => (measured ??= (await drawn(page, seen, "Plain Film")).pixels);

await scenario("ASS is drawn in the fallback font on the first play of a film with fonts, and in the attached font after", async ({ page, seen }) => {
  const plain = await fallbackPixels(page, seen);
  const first = await drawn(page, seen, "Font Film");
  if (first.fonts.length) throw new Error(`a font was listed before it was stored: ${JSON.stringify(first.fonts)}`);
  if (!sameText(first.pixels, plain)) throw new Error(`the first play covers ${first.pixels} pixels, the fallback ${plain}`);
  await fontsJobDone("Font Film");
  const later = await drawn(page, seen, "Font Film");
  const [font] = later.fonts;
  if (later.fonts.length !== 1 || font.status !== 200 || font.type !== "font/ttf" || font.bytes < 10000) throw new Error(`the font: ${JSON.stringify(later.fonts)}`);
  if (sameText(later.pixels, plain)) throw new Error(`the text covers ${plain} pixels without the font and ${later.pixels} with it`);
  noErrors(seen);
});

await scenario("a film whose fonts job failed for good is never listed fonts, and ASS is drawn in the fallback", async ({ page, seen }) => {
  const plain = await fallbackPixels(page, seen);
  const first = await drawn(page, seen, "Cracked Fonts Film");
  await fontsJobDone("Cracked Fonts Film");
  const later = await drawn(page, seen, "Cracked Fonts Film");
  if (first.fonts.length || later.fonts.length) throw new Error(`fonts were listed: ${JSON.stringify([...first.fonts, ...later.fonts])}`);
  if (!sameText(first.pixels, plain) || !sameText(later.pixels, plain)) throw new Error(`the text covers ${first.pixels} and ${later.pixels} pixels, the fallback ${plain}`);
  noErrors(seen);
});

// The font's object is gone from the asset store while the film lists it. The
// attachment is answered with an empty font, the track is drawn in the fallback, and the font
// is put back for the next play.
await scenario("a listed font gone from the store: ASS is still drawn, and the font comes back", async ({ page, seen }) => {
  const plain = await fallbackPixels(page, seen);
  await drawn(page, seen, "Vanishing Fonts Film");
  await fontsJobDone("Vanishing Fonts Film");
  const held = await drawn(page, seen, "Vanishing Fonts Film");
  if (held.fonts.length !== 1 || held.fonts[0].status !== 200) throw new Error(`the font: ${JSON.stringify(held.fonts)}`);
  const objects = `${process.env.E2E_ASSETS}/fonts`;
  const [name, ...others] = readdirSync(objects);
  if (!name || others.length) throw new Error(`font objects: ${[name, ...others]}`);
  rmSync(`${objects}/${name}`);
  // A viewer who holds the font is told 304 without the store being read, so the
  // lost font is met by one who doesn't: a browser with a cache of its own.
  const other = await viewer();
  let lost;
  try {
    lost = await drawn(other.page, other.seen, "Vanishing Fonts Film");
  } finally {
    await other.context.close().catch(() => {});
  }
  if (lost.fonts.length !== 1 || lost.fonts[0].status !== 200 || lost.fonts[0].bytes !== 0) throw new Error(`the lost font: ${JSON.stringify(lost.fonts)}`);
  if (!sameText(lost.pixels, plain)) throw new Error(`the text covers ${lost.pixels} pixels, the fallback ${plain}`);
  await waitFor("the font put back", async () => existsSync(`${objects}/${name}`));
  const back = await drawn(page, seen, "Vanishing Fonts Film");
  if (back.fonts.length !== 1 || back.fonts[0].status !== 200 || sameText(back.pixels, plain)) throw new Error(`after it came back: ${JSON.stringify(back.fonts)}, ${back.pixels} pixels`);
  noErrors(seen);
});

// The store gives back half of the font, as an S3 that ends a body early does.
// The route answers with the empty font of a lost one, not a cut font or an error.
await scenario("a listed font cut short by the store: ASS is still drawn", async ({ page, seen }) => {
  const plain = await fallbackPixels(page, seen);
  // The font is stored by the first play of the film; the scenario above may not have run on this shard.
  await drawn(page, seen, "Vanishing Fonts Film");
  await fontsJobDone("Vanishing Fonts Film");
  const objects = `${process.env.E2E_ASSETS}/fonts`;
  const [name, ...others] = readdirSync(objects);
  if (!name || others.length) throw new Error(`font objects: ${[name, ...others]}`);
  truncateSync(`${objects}/${name}`, statSync(`${objects}/${name}`).size / 2);
  // A fresh viewer: one who holds the font is told 304 without the store being read.
  const other = await viewer();
  let cut;
  try {
    cut = await drawn(other.page, other.seen, "Vanishing Fonts Film");
  } finally {
    await other.context.close().catch(() => {});
  }
  if (cut.fonts.length !== 1 || cut.fonts[0].status !== 200 || cut.fonts[0].bytes !== 0) throw new Error(`the cut font: ${JSON.stringify(cut.fonts)}`);
  if (!sameText(cut.pixels, plain)) throw new Error(`the text covers ${cut.pixels} pixels, the fallback ${plain}`);
  noErrors(seen);
});

// The films with image tracks are black, with a yellow bar on the track's
// 1920x1080 canvas: at the top from 20 to 26 seconds, at the bottom from 60 to 66
// and in the middle from 100 to 106. A cue is looked at a few seconds in, after
// a seek to before it, so the run a transcode restarts at starts before the cue
// and reads it (a restart inside a cue draws it only from the next, DOMAINS 48).
async function seeCue(page, from, until, where, thirds) {
  await seek(page, from);
  await waitFor(`playback past ${until}`, async () => (await time(page)) >= until, 60000);
  expectBright(await thirds(), where);
}
const cues = [[18, 22, "top"], [50, 52, null], [58, 62, "bottom"], [98, 102, "middle"]];

// A PGS track listed as Jellyfin lists it, fetched as the file it is by a
// jellyfin-web whose viewer has PGS rendering on, and drawn on screen where and
// when its cues say, with no PlaybackInfo, transcode job or playlist for the
// pick and nothing drawn into the picture. The film plays as it is or, with
// transcoded, is transcoded anyway (its audio is AC3) and the track is still
// the client's to draw. With external the track is a .sup beside the film, which a
// worker reads itself, however it does that.
function pgsDrawnByTheClient(name, library, title, transcoded, external = false) {
  return scenario(name, async ({ page, seen }) => {
    await page.evaluate(() => localStorage.setItem("subtitlerenderpgs", "true"));
    const supJobs = Number(sql("SELECT count(*) FROM subtitle_jobs WHERE kind = 'sup'"));
    await play(page, library, title);
    const first = seen.infos.at(-1);
    if (!first.request.DeviceProfile.SubtitleProfiles.some((p) => p.Format === "pgssub" && p.Method === "External")) throw new Error("the viewer's setting didn't reach the profile");
    if (transcoded !== Boolean(first.source.TranscodingUrl)) throw new Error(`transcoded should be ${transcoded}: ${first.source.TranscodingUrl}`);
    if (first.source.TranscodingUrl && /SubtitleStreamIndex/.test(first.source.TranscodingUrl)) throw new Error(`the transcode was told to draw it: ${first.source.TranscodingUrl}`);
    const tracks = first.source.MediaStreams.filter((s) => s.Type === "Subtitle");
    const [pgs] = tracks;
    if (tracks.length !== 1 || pgs.Index !== 2 || pgs.Codec !== "PGSSUB" || pgs.DeliveryMethod !== "External" || pgs.IsExternalUrl !== false || pgs.IsTextSubtitleStream !== false ||
      pgs.SupportsExternalStream !== true || pgs.Language !== "eng" || pgs.Width !== 1920 || pgs.Height !== 1080 ||
      !/^\/Videos\/[0-9a-f-]{36}\/[0-9a-f]{32}\/Subtitles\/2\/0\/Stream\.pgssub\?ApiKey=\w+$/i.test(pgs.DeliveryUrl)) {
      throw new Error(`tracks: ${JSON.stringify(tracks)}`);
    }
    if (external && (pgs.IsExternal !== true || pgs.DisplayTitle !== "English - PGSSUB - External")) throw new Error(`the file beside the film: ${JSON.stringify(pgs)}`);
    await advances(page, 3);
    await pickSubtitle(page, -1); // Whatever the client started with, begin from off.
    await new Promise((resolve) => setTimeout(resolve, 1500));
    expectBright(await screenThirds(page), null);
    const infos = seen.infos.length, jobs = jobCount(), playlists = seen.playlists.length;

    await pickSubtitle(page, 2);
    await waitFor("the PGS file", async () => seen.subtitles.some((r) => r.index === 2 && r.status === 200));
    const files = seen.subtitles.filter((r) => r.index === 2);
    if (files.some((r) => !/Stream\.pgssub$/i.test(r.path) || r.type !== "application/octet-stream") || !files.some((r) => r.status === 206)) {
      throw new Error(`the PGS file was fetched as ${JSON.stringify(files)}`);
    }
    for (const [from, until, where] of cues) await seeCue(page, from, until, where, () => screenThirds(page));

    if (seen.infos.length !== infos || jobCount() !== jobs || seen.playlists.length !== playlists) {
      throw new Error(`picking the track caused work: ${seen.infos.length - infos} PlaybackInfo, ${jobCount() - jobs} jobs, ${seen.playlists.length - playlists} playlists`);
    }
    if (!external && Number(sql("SELECT count(*) FROM subtitle_jobs WHERE kind = 'sup'")) - supJobs !== 1) throw new Error("the file was not copied out of the film by one job");
    if (ffmpegRuns().some((r) => r.burns)) throw new Error(`a transcode draws a track the client draws: ${JSON.stringify(ffmpegRuns())}`);
    if (!transcoded && ffmpegRuns().length) throw new Error("ffmpeg ran for a direct play");
    noErrors(seen);
    await leave(page);
  });
}

await pgsDrawnByTheClient("POSIX direct play of PGS: jellyfin-web draws the track from the file where its cues say", "Transcode", "Pgs Film", false);
await pgsDrawnByTheClient("POSIX transcode with PGS: jellyfin-web draws the track from the file, and the transcode doesn't", "Transcode", "Pgs Transcode Film", true);
await pgsDrawnByTheClient("S3 direct play of PGS: jellyfin-web draws the track from the file where its cues say", "Cloud", "Pgs Cloud Film", false);

// An image track that the client doesn't draw is drawn into the picture by the
// transcode: PGS with the viewer's default settings, and VobSub whatever they
// are. It is read from the film by the transcode itself, so no subtitle job is
// made, and no file is fetched. The bar is in the picture at the cue's time
// after far seeks, which restart the job part-way in. With external the track is
// a file beside the film (a .sup, or a .sub and .idx pair), which a worker
// reads itself; whether that takes a subtitle job is its own business.
function imageBurnedIn(name, library, title, codec, renderPgs = false, external = false) {
  return scenario(name, async ({ page, seen }) => {
    if (renderPgs) await page.evaluate(() => localStorage.setItem("subtitlerenderpgs", "true"));
    const subtitleJobs = subtitleJobCount();
    const logged = ffmpegLines().length;
    await play(page, library, title);
    const first = seen.infos.at(-1);
    const [track, ...others] = first.source.MediaStreams.filter((s) => s.Type === "Subtitle");
    if (others.length || track.Index !== 2 || track.Codec !== codec || track.DeliveryMethod !== "Encode" || track.DeliveryUrl !== undefined || track.IsTextSubtitleStream !== false) {
      throw new Error(`tracks: ${JSON.stringify(first.source.MediaStreams.filter((s) => s.Type === "Subtitle"))}`);
    }
    if (first.source.TranscodingUrl) throw new Error(`transcoding for no subtitle: ${first.source.TranscodingUrl}`);
    if (external && track.IsExternal !== true) throw new Error(`the file beside the film: ${JSON.stringify(track)}`);

    // The burn-in begins where the viewer is: at 12 s the run starts between the
    // film's start and the first cue and reads the cue itself. A seek made once
    // the first run is playing cannot do that, whatever the machine's speed: the
    // job is waited for when it is within catchUpSegments of the segment asked
    // for, and past its first segment it always is (JS-099).
    await seek(page, 12);
    await chooseBurn(page, seen, 2);
    if (!ffmpegRuns().some((r) => r.burns)) throw new Error(`no ffmpeg draws the subtitle: ${JSON.stringify(ffmpegRuns())}`);
    for (const [from, until, where] of cues) await seeCue(page, from, until, where, () => brightThirds(page, true));
    const starts = ffmpegLines().slice(logged).filter((line) => /overlay=eof_action/.test(line)).map((line) => Number(/-start_number (\d+)/.exec(line)?.[1]));
    if (!starts.some((n) => n > 0 && n < 5)) throw new Error(`no run began before the first cue: runs from segments ${starts}`);

    await chooseBurn(page, seen, -1);
    await seek(page, 60);
    expectBright(await brightThirds(page, true), null);
    await waitFor("no ffmpeg to draw a subtitle with them off", async () => !ffmpegRuns().some((r) => r.burns), 20000);
    if (seen.subtitles.length) throw new Error("subtitle files were fetched while burning in");
    if (!external && subtitleJobCount() !== subtitleJobs) throw new Error(`${subtitleJobCount() - subtitleJobs} subtitle jobs for a track the transcode reads itself`);
    noErrors(seen);
    await leave(page);
  });
}

await imageBurnedIn("POSIX burn-in of PGS for a viewer whose client doesn't draw it: the bar is in the picture at the right time, after far seeks", "Transcode", "Pgs Film", "PGSSUB");
await imageBurnedIn("S3 burn-in of PGS for a viewer whose client doesn't draw it: the bar is in the picture at the right time, after far seeks", "Cloud", "Pgs Cloud Film", "PGSSUB");

// JS-071. A burnt image track is shared by workers like any slow re-encode, and
// a helper whose chunk begins inside a cue draws it from its first frame. Every
// worker's ffmpeg reads at 0.7 of the film's speed, so a viewer who seeks to
// 56 s gets a helper at segment 16 (64 s, the next chunk start) while the first
// job is still on its way; the bottom bar is on from 60 to 66 s, across the
// seam, and is looked at just after the helper's first frame and again two
// seconds on.
await scenario("a slow burn-in of PGS is shared by workers, and the cue is on screen across a helper's chunk start", async ({ page, seen }) => {
  const launch = process.env.E2E_WORKER;
  if (!launch) throw new Error("the parallel scenario needs E2E_WORKER (test/e2e/run.sh sets it)");
  const rate = `${dirname(launch)}/rate`;
  writeFileSync(rate, "0.7");
  const workers = [];
  try {
    for (let i = 0; i < 2; i++) workers.push(spawn(launch, [], { stdio: "ignore" }));
    await waitFor("three workers to hold their locks", async () => sql(`SELECT count(*) FROM transcode_nodes n WHERE slots > 0 AND locked AND EXISTS (SELECT 1 FROM pg_locks l
      WHERE l.locktype = 'advisory' AND l.classid = 27219 AND l.objsubid = 2 AND l.objid::bigint = hashtext(n.id)::bigint & 4294967295)`) === "3", 60000);
    await play(page, "Transcode", "Pgs Parallel Film");
    await chooseBurn(page, seen, 2);
    await page.evaluate(() => { document.querySelector("video").currentTime = 56; });
    await waitFor("a helper at the chunk start after the cue's start", async () => sql("SELECT count(*) FROM transcode_jobs WHERE helper AND start_segment = 16") !== "0", 90000);
    // The cue began at 60 s, before the helper's chunk (64 s): it is on screen before the seam.
    await waitFor("playback to reach the second before the helper's chunk", async () => (await time(page)) >= 62.5, 90000);
    if ((await time(page)) >= 63.9) throw new Error("playback was already past 63.9 s when the picture before the helper's chunk was looked at");
    expectBright(await brightThirds(page, true), "bottom");
    await waitFor("playback to reach the helper's first frame", async () => (await time(page)) >= 64.2, 90000);
    expectBright(await brightThirds(page, true), "bottom");
    await waitFor("playback two seconds on", async () => (await time(page)) >= 65.5, 30000);
    if ((await time(page)) < 66) expectBright(await brightThirds(page, true), "bottom");
    if (sql("SELECT count(*) FROM transcode_jobs WHERE helper AND start_segment = 16 AND next_segment > 16") === "0") throw new Error("the helper at segment 16 made no segment, so the seam was not played from it");
    noErrors(seen);
    await leave(page);
  } finally {
    for (const w of workers) w.kill("SIGKILL");
    rmSync(rate, { force: true });
  }
});
await imageBurnedIn("POSIX burn-in of VobSub: the bar is in the picture at the right time, after far seeks", "Transcode", "Vobsub Film", "DVDSUB");
await imageBurnedIn("POSIX burn-in of VobSub for a viewer whose client draws PGS and VobSub: it is still burnt in", "Transcode", "Vobsub Render Film", "DVDSUB", true);
await imageBurnedIn("S3 burn-in of VobSub: the bar is in the picture at the right time, after far seeks", "Cloud", "Vobsub Cloud Film", "DVDSUB");
await imageBurnedIn("POSIX burn-in of DVB: the bar is in the picture at the right time, after far seeks", "Transcode", "Dvb Film", "DVBSUB");

// The same with the track in a file beside the film. A .sup is the file a client
// that draws PGS fetches, as for a track inside the film, and is drawn into a
// transcode for any other; a .sub and .idx pair is one VobSub track that is
// drawn into a transcode whatever the client says it draws, as Jellyfin 12.4 does
// for files (compat/captures/external-image-subtitles-12.4.md).
await pgsDrawnByTheClient("POSIX direct play of a .sup beside the film: jellyfin-web draws the track from the file where its cues say", "Transcode", "Sup Beside Film", false, true);
await pgsDrawnByTheClient("POSIX transcode with a .sup beside the film: jellyfin-web draws the track from the file, and the transcode doesn't", "Transcode", "Sup Beside Transcode Film", true, true);
await pgsDrawnByTheClient("S3 direct play of a .sup beside the film: jellyfin-web draws the track from the file where its cues say", "Cloud", "Sup Beside Cloud Film", false, true);
await imageBurnedIn("POSIX burn-in of a .sup beside the film for a viewer whose client doesn't draw it: the bar is in the picture at the right time, after far seeks", "Transcode", "Sup Beside Film", "PGSSUB", false, true);
await imageBurnedIn("S3 burn-in of a .sup beside the film for a viewer whose client doesn't draw it: the bar is in the picture at the right time, after far seeks", "Cloud", "Sup Beside Cloud Film", "PGSSUB", false, true);
await imageBurnedIn("POSIX burn-in of a .sub and .idx pair beside the film: the bar is in the picture at the right time, after far seeks", "Transcode", "Idx Beside Film", "DVDSUB", false, true);
await imageBurnedIn("POSIX burn-in of a .sub and .idx pair for a viewer whose client draws PGS and VobSub: it is still burnt in", "Transcode", "Idx Beside Render Film", "DVDSUB", true, true);
await imageBurnedIn("S3 burn-in of a .sub and .idx pair beside the film: the bar is in the picture at the right time, after far seeks", "Cloud", "Idx Beside Cloud Film", "DVDSUB", false, true);

// A .sub with no .idx and an .idx with no .sub are no track: the film is listed
// and plays, with nothing in its subtitle menu.
await scenario("a .sub without its .idx and an .idx without its .sub are not listed, and the film plays", async ({ page, seen }) => {
  await play(page, "Transcode", "Orphan Beside Film");
  const tracks = seen.infos.at(-1).source.MediaStreams.filter((s) => s.Type === "Subtitle");
  if (tracks.length) throw new Error(`tracks: ${JSON.stringify(tracks)}`);
  await advances(page, 2);
  noErrors(seen);
  await leave(page);
});

// Choosing a track to draw into the picture replaces the stream: PlaybackInfo,
// DELETE, a new master. Returns the PlaybackInfo.
async function chooseBurn(page, seen, index) {
  const count = seen.infos.length, deletes = seen.deletes.length, playlists = seen.playlists.length;
  const replaces = Boolean(seen.infos.at(-1).source.TranscodingUrl); // A direct play has nothing to delete.
  await pickSubtitle(page, index);
  const info = await waitFor(`a PlaybackInfo for track ${index}`, async () => seen.infos.length > count && seen.infos.at(-1));
  if (Number(info.request.SubtitleStreamIndex) !== index) throw new Error(`asked for ${info.request.SubtitleStreamIndex}`);
  const url = info.source.TranscodingUrl;
  const query = url ? new URL(url, base).searchParams : null;
  if (index >= 0) {
    if (query?.get("SubtitleStreamIndex") !== String(index) || !/SubtitleCodecNotSupported/.test(query.get("TranscodeReasons") ?? "")) throw new Error(`TranscodingUrl: ${url}`);
  } else if (query?.has("SubtitleStreamIndex")) {
    throw new Error(`off still names a subtitle: ${url}`);
  }
  if (replaces) await waitFor("DELETE ActiveEncodings", async () => seen.deletes.length > deletes);
  if (url) await waitFor("a new master playlist", async () => seen.playlists.length > playlists);
  await advances(page, 2);
  return info;
}

// Burn-in, with the settings of the capture: "Burn subtitles" set to all, which
// sends no subtitle profile, and "Always burn in subtitle when transcoding".
// The film is black, with the ASS track's text at the top from 20 to 40
// seconds, at the bottom from 60 to 80 and in the middle from 100 to 120, and
// the SubRip track's at the bottom from 60 to 80.
function burnedIn(name, library, title) {
  return scenario(name, async ({ page, seen }) => {
    await page.evaluate(() => {
      localStorage.setItem("subtitleburnin", "all");
      localStorage.setItem("alwaysBurnInSubtitleWhenTranscoding", "true");
    });
    await play(page, library, title);
    const first = seen.infos.at(-1);
    if (first.request.DeviceProfile.SubtitleProfiles.length !== 0 || first.request.AlwaysBurnInSubtitleWhenTranscoding !== true) throw new Error("the settings didn't reach PlaybackInfo");
    const marked = first.source.MediaStreams.filter((s) => s.Type === "Subtitle");
    if (marked.length !== 2 || marked.some((s) => s.DeliveryMethod !== "Encode" || s.DeliveryUrl !== undefined)) throw new Error(`tracks: ${JSON.stringify(marked)}`);

    const choose = (index) => chooseBurn(page, seen, index);

    await choose(3);
    if (!ffmpegRuns().some((r) => r.burns)) throw new Error(`no ffmpeg draws the subtitle: ${JSON.stringify(ffmpegRuns())}`);
    await seek(page, 25);
    expectBright(await brightThirds(page), "top");
    await seek(page, 50);
    expectBright(await brightThirds(page), null);
    // A far seek restarts the job part-way in, and the text is where it belongs.
    await seek(page, 70);
    expectBright(await brightThirds(page), "bottom");
    if (!ffmpegRuns().some((r) => r.burns && r.start > 10)) throw new Error(`the seek didn't restart the job: ${JSON.stringify(ffmpegRuns())}`);
    await seek(page, 110);
    expectBright(await brightThirds(page), "middle");

    // The other track is another job, and carries only its own cues.
    await seek(page, 70);
    await choose(2);
    await seek(page, 70);
    expectBright(await brightThirds(page), "bottom");
    await seek(page, 30);
    expectBright(await brightThirds(page), null);

    // Off: no text, and nothing is drawn into the picture.
    await choose(-1);
    await seek(page, 70);
    expectBright(await brightThirds(page), null);
    await waitFor("no ffmpeg to draw a subtitle with them off", async () => !ffmpegRuns().some((r) => r.burns), 20000);
    if (seen.subtitles.length) throw new Error("subtitle files were fetched while burning in");
    noErrors(seen);
    await leave(page);
  });
}

await burnedIn("POSIX burn-in: text is in the picture at the right time, after far seeks and track changes", "Transcode", "Phi Film");
await burnedIn("S3 burn-in: text is in the picture at the right time, after far seeks and track changes", "Cloud", "Omega Film");
await burnedIn("POSIX burn-in of subtitle files beside the film: text is in the picture at the right time", "Transcode", "Burn Sub Film");
await burnedIn("S3 burn-in of subtitle files beside the film: text is in the picture at the right time", "Cloud", "Beside Burn Film");

// JS-103: a viewer who turns on a track the browser has not fetched while Postgres is stopped sees its cues, from a
// stream replica that served the viewer nothing before: a text track, and an ASS track with its font. The tracks and the
// fonts are in the asset store because another browser played the film and picked the track first, and the fonts job has
// run (a film's fonts are listed to the player only once it has). The viewer starts with subtitles off, the snapshot
// holds its session, Postgres is stopped, the router sends the stream role to the second replica only, and the pick is
// the first request for the track. The player must not show a dialog, ask for PlaybackInfo again or be answered 400 or
// more, and Postgres is still stopped when the cues are on screen.
const OUTAGE_SNAPSHOT_REFRESHED = 22000; // Two refresh intervals and a margin (DOMAINS 37 says at most 10 seconds each).
for (const flavour of [
  { name: "a text track", film: "Outage Pick Film", index: 2, shown: async (page) => (await activeText(page)) === "English line 1", at: 12 },
  { name: "an ASS track and its font", film: "Outage Font Film", index: 2, fonts: true, shown: async (page) => (await assPixels(page)) > 100, at: 5 },
]) {
  await scenario(`Postgres stopped: a viewer who turns on ${flavour.name} the browser has not fetched sees it, from a stream replica that served the viewer nothing before`, async (v) => {
    const { page, seen } = v;
    const warm = await viewer();
    try {
      await play(warm.page, "Transcode", flavour.film);
      await pickSubtitle(warm.page, flavour.index);
      await seek(warm.page, flavour.at);
      await waitFor("the track to be shown to the first viewer", async () => flavour.shown(warm.page), 60000);
      if (flavour.fonts) await fontsJobDone(flavour.film);
      await leave(warm.page);
    } finally {
      await warm.context.close().catch(() => {});
    }
    let stopped = false;
    try {
      await withTwoStreamReplicas(base, async ({ a, b }) => {
        await routerControl(base, { fallback: true, stream: [a] });
        await play(page, "Transcode", flavour.film);
        const listed = seen.infos.at(-1).source.MediaAttachments?.length ?? 0;
        if (flavour.fonts && !listed) throw new Error("the film lists no font: the fonts job has not stored them");
        await pickSubtitle(page, -1).catch(async (error) => { throw new Error(`${error.message}; the page: ${page.url()}, buttons ${JSON.stringify(await page.evaluate(() => [...document.querySelectorAll("button")].filter((b) => b.offsetParent).map((b) => b.className.slice(0, 40)).slice(0, 12)))}, a video at ${await time(page)} s`); });
        await advances(page, 2);
        // Paused while the snapshot is refreshed: the font film lasts 30 seconds, which the wait would play to its end.
        await page.evaluate(() => { window.__awayVideo = document.querySelector("video"); document.querySelector("video").pause(); });
        const asked = seen.infos.length;
        const fetched = seen.subtitles.length, fonts = seen.attachments.length;
        await new Promise((resolve) => setTimeout(resolve, OUTAGE_SNAPSHOT_REFRESHED));
        const downAt = Date.now();
        await postgres("stop");
        stopped = true;
        await routerControl(base, { stream: [b] });
        await pickSubtitle(page, flavour.index).catch(async (error) => { throw new Error(`${error.message}; the page: ${page.url()}, buttons ${JSON.stringify(await page.evaluate(() => [...document.querySelectorAll("button")].filter((b) => b.offsetParent).map((b) => b.className.slice(0, 40)).slice(0, 12)))}, a video at ${await time(page)} s, paused ${await page.evaluate(() => document.querySelector("video")?.paused)}, ended ${await page.evaluate(() => document.querySelector("video")?.ended)}, dialog ${await page.getByText("Playback Error").count()}`); });
        await waitFor("the track to be answered 200 while Postgres is stopped", async () => seen.subtitles.slice(fetched).find((r) => r.index === flavour.index && r.status === 200), 60000);
        await page.evaluate(() => document.querySelector("video").play());
        await seek(page, flavour.at);
        await waitFor("the cues to be shown", async () => flavour.shown(page), 60000);
        if (flavour.fonts) {
          await waitFor("the font to be answered", async () => seen.attachments.length > fonts, 30000);
          const bad = seen.attachments.slice(fonts).filter((f) => f.status !== 200 || f.bytes < 10000);
          if (bad.length || seen.attachments.length - fonts !== listed) throw new Error(`the fonts: ${JSON.stringify(seen.attachments.slice(fonts))}, ${listed} listed`);
        }
        const running = (await exec_("docker", ["inspect", "-f", "{{.State.Running}}", process.env.E2E_PG])).trim();
        if (running === "true") throw new Error("Postgres was running again before the checks: the cues did not come from the replica");
        if (await page.getByText("Playback Error").count()) throw new Error("the viewer got a Playback Error dialog");
        if (!(await page.evaluate(() => document.querySelector("video") === window.__awayVideo))) throw new Error("the player was replaced");
        if (seen.infos.length !== asked) throw new Error(`the client asked for PlaybackInfo again (${seen.infos.length} times, not ${asked})`);
        const refused = seen.answers.filter((x) => x.at > downAt && x.status >= 400);
        if (refused.length) throw new Error(`answers of 400 or more: ${JSON.stringify(refused.slice(0, 4).map((x) => `${x.status} ${new URL(x.url).pathname}`))}`);
        stopped = false;
        await postgres("start");
        await waitFor("every replica to be ready again", () => everyReplicaReady(base), 60000);
        await leave(page);
      });
    } finally {
      if (stopped) await postgres("start");
      await routerControl(base, { reset: true });
      await waitFor("every replica to be ready again", () => everyReplicaReady(base), 60000);
    }
  });
}

await runLater();
await browser.close();
if (failures.length) {
  console.log("\nFAILED");
  for (const failure of failures) console.log(`  - ${failure}`);
  shardLine();
  process.exit(1);
}
console.log("\nPASS");
shardLine();
