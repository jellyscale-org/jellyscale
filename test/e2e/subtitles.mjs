// Phase 3 gate: unmodified jellyfin-web shows text subtitles, from POSIX and S3,
// on direct play and on a transcode, and draws them into the picture when the
// viewer's settings ask for it. The tracks are in the film or in files beside
// it, and ASS is drawn in the fonts attached to the film. PGS tracks are drawn by
// jellyfin-web from the file when its viewer has that on, and into the picture
// by a transcode otherwise, as VobSub always is. Needs the stack test/e2e/run.sh starts.
// Usage: E2E_SCRATCH=<worker scratch dir> E2E_PG=<postgres container> node subtitles.mjs <base_url> <user> <password>
// Run from a directory with playwright-core installed.
import { execFile, spawn } from "node:child_process";
import { promisify } from "node:util";
import { existsSync, readFileSync, readdirSync, rmSync, statSync, truncateSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { createRequire } from "node:module";
// The shared viewer records each page's responses for the checker (conform.mjs).
import { advances, configure, ffmpegRuns, scenario, seek, sql, time, viewer, waitFor } from "./player.mjs";
import { runLater, schedule, shardLine } from "./shard.mjs";
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
configure({ name: "subtitles", base, user, password, browser, failures, ffmpegWait: 90000, reasonLines: 3 });

// Scenarios that do not test the subtitle mode start with none, whatever an
// earlier gate left saved. A mode is saved, not reset: POSTing {} changes nothing.
async function startWithNoSubtitleMode() {
  const auth = 'MediaBrowser Client="e2e", Device="subtitles", DeviceId="subtitles-gate", Version="1.0"';
  const signedIn = await fetch(`${base}/Users/AuthenticateByName`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: auth },
    body: JSON.stringify({ Username: user, Pw: password }),
  });
  if (!signedIn.ok) throw new Error(`signing in for the gate's settings: ${signedIn.status} ${await signedIn.text()}`);
  const session = await signedIn.json();
  const headers = { Authorization: `MediaBrowser Token="${session.AccessToken}"` };
  const me = await fetch(`${base}/Users/Me`, { headers });
  if (!me.ok) throw new Error(`reading the gate's configuration: ${me.status} ${await me.text()}`);
  const configuration = (await me.json()).Configuration;
  const saved = await fetch(`${base}/Users/${session.User.Id}/Configuration`, {
    method: "POST",
    headers: { ...headers, "Content-Type": "application/json" },
    body: JSON.stringify({ ...configuration, SubtitleMode: "None" }),
  });
  if (!saved.ok) throw new Error(`saving the gate's configuration: ${saved.status} ${await saved.text()}`);
}
await startWithNoSubtitleMode();

const exec_ = async (cmd, args) => (await promisify(execFile)(cmd, args)).stdout;
// Specs with a job, not jobs: a seek past what the job has made (transcodeLead)
// starts another job of the same spec, whereas picking a track that is drawn into
// the picture would be another spec.
const jobCount = () => Number(sql("SELECT count(DISTINCT spec_hash) FROM transcode_jobs"));
const subtitleJobCount = () => Number(sql("SELECT count(*) FROM subtitle_jobs"));
// Every subtitle job but one kind: since JS-089 a burnt image track is also stored in the
// background (kind 'images') so that a later seek into a long cue is cheap. No run waits on that
// job (TestNoRunWaitsForTheExtractionAndEachDrawsAsJS071Does, decision D-004); every other job, a 'sup' extraction above all, still
// fails a burn the transcode reads itself.
const jobsOtherThanImages = () => Number(sql("SELECT count(*) FROM subtitle_jobs WHERE kind <> 'images'"));
const imageJobCount = () => Number(sql("SELECT count(*) FROM subtitle_jobs WHERE kind = 'images'"));

// Every ffmpeg command line the worker has run, from the shim's log beside the
// server's.
const ffmpegLines = () => readFileSync(join(dirname(process.env.E2E_LOG), "ffmpeg.log"), "utf8").split("\n").filter(Boolean);

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
    const subtitleJobs = jobsOtherThanImages();
    const imageJobs = imageJobCount();
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
    if (!external && jobsOtherThanImages() !== subtitleJobs) throw new Error(`${jobsOtherThanImages() - subtitleJobs} subtitle jobs for a track the transcode reads itself`);
    // At most one: the film's image tracks are queued in the background and may land after the read, or an
    // earlier scenario stored them already.
    if (imageJobCount() - imageJobs > 1) throw new Error(`${imageJobCount() - imageJobs} image track jobs for one play`);
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
    if (!ffmpegRuns().some((r) => r.burns && r.startNumber > 10)) throw new Error(`the seek didn't restart the job: ${JSON.stringify(ffmpegRuns())}`);
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

// JS-119: a viewer who opted into hiding the tracks outside their languages (audio and subtitle eng, both switches on) is
// offered, by unmodified jellyfin-web, only their tracks in the player's menus and in the details page's media info;
// Babel Film's other tracks are listed as placeholders of Type Data, which no menu or dialog shows. Picking a kept track
// by hand shows its cue. With "show all tracks" on and the page reloaded the menus list every track again. Babel Film has
// audio 1 eng, 2 deu, 3 fra, 4 untagged and subtitles 5 to 12 and the two files beside it as 13 and 14 (test/fixtures/babel.sh);
// no scenario before this one plays it, so its text tracks are read for the first time here.
const apiCall = (page, method, path, body) => page.evaluate(async ([method, path, body]) => {
  const response = await fetch(path, { method, body, headers: { "Content-Type": "application/json", Authorization: `MediaBrowser Token="${window.ApiClient.accessToken()}"` } });
  return { status: response.status, text: await response.text() };
}, [method, path, body ?? null]);

// The whole saved configuration, to put back exactly: POSTing {} changes nothing.
async function savedConfiguration(page) {
  const answer = await apiCall(page, "GET", "/Users/Me");
  if (answer.status !== 200) throw new Error(`reading the configuration: ${answer.status} ${answer.text}`);
  return JSON.parse(answer.text).Configuration;
}

async function restoreConfiguration(page, configuration) {
  const userId = await page.evaluate(() => window.ApiClient.getCurrentUserId());
  const answer = await apiCall(page, "POST", `/Users/${userId}/Configuration`, JSON.stringify(configuration));
  if (answer.status !== 204) throw new Error(`restoring the configuration: ${answer.status} ${answer.text}`);
}

// The entries of the player's audio or subtitle menu: the data-id of each, but not Off or the secondary subtitles item.
async function menuIds(page, button, seen) {
  await waitFor(`the ${button} menu`, async () => {
    await page.mouse.move(400 + Math.random() * 300, 300 + Math.random() * 200);
    return page.click(button, { timeout: 1500 }).then(() => true, () => false);
  }, 30000);
  await page.waitForSelector(".actionSheetMenuItem", { timeout: 10000 });
  const ids = await page.evaluate(() => [...document.querySelectorAll(".actionSheetMenuItem")].map((e) => e.dataset.id));
  // Closing a sheet without choosing makes jellyfin-web itself log "ActionSheet closed without resolving": not the server's error, so it is dropped here only.
  await page.mouse.click(5, 5);
  await waitFor("the menu to close", async () => (await page.locator(".actionSheetMenuItem").count()) === 0, 10000);
  seen.errors = seen.errors.filter((e) => !/ActionSheet closed without resolving/.test(e));
  return ids.filter((id) => id !== "-1" && id !== "secondarysubtitle").map(Number).sort((a, b) => a - b);
}

await scenario("a viewer who hides the tracks outside their languages sees only their tracks in the menus and the media info, and can pick one by hand", async ({ page, seen }) => {
  const userId = await page.evaluate(() => window.ApiClient.getCurrentUserId());
  const ok = async (method, path, body, want) => {
    const answer = await apiCall(page, method, path, body);
    if (answer.status !== want) throw new Error(`${method} ${path}: ${answer.status} ${answer.text}`);
  };
  const open = async () => {
    await page.goto(`${base}/web/`);
    await page.waitForSelector(".card", { timeout: 30000 });
    await page.locator(".card:visible", { hasText: "Languages" }).first().click();
    await page.locator(".cardText a:visible", { hasText: "Babel Film" }).first().click();
    await page.waitForSelector(".btnPlay:visible", { timeout: 30000 });
  };
  // The streams the media info dialog of the details page lists, by kind.
  const mediaInfo = async () => {
    await page.click(".btnMoreCommands:visible");
    await page.click('.actionSheetMenuItem[data-id="moremediainfo"]');
    await page.waitForSelector(".mediaInfoStream", { timeout: 10000 });
    const kinds = await page.evaluate(() => [...document.querySelectorAll(".mediaInfoStream")].map((e) => e.innerText.split("\n")[0].trim()));
    await page.goBack();
    await page.waitForSelector(".btnPlay:visible", { timeout: 30000 });
    return kinds;
  };
  const playAndMenus = async (seen) => {
    await page.locator(".btnPlay:visible").first().click();
    await waitFor("video to start", async () => (await time(page)) > 0.5, 60000);
    return { audio: await menuIds(page, ".btnAudio", seen), subtitles: await menuIds(page, ".btnSubtitles", seen) };
  };
  const same = (got, want, what) => { if (got.join() !== want.join()) throw new Error(`${what}: ${got.join()}, want ${want.join()}`); };
  const previous = await savedConfiguration(page);
  try {
    await ok("POST", `/Users/${userId}/Configuration`, JSON.stringify({ AudioLanguagePreference: "eng", SubtitleLanguagePreference: "eng" }), 204);
    await ok("PATCH", "/jellyscale/v1/track-preferences", JSON.stringify({ HideOtherAudio: true, HideOtherSubtitles: true, ShowAllTracks: false }), 200);

    await open();
    const kinds = await mediaInfo();
    const subtitleStreams = kinds.filter((k) => k === "Subtitle").length;
    if (subtitleStreams !== 4) throw new Error(`the media info lists ${subtitleStreams} subtitle streams (${kinds.join(", ")}), want 4 (the viewer's own; 10 without hiding)`);
    if (kinds.some((k) => k !== "Video" && k !== "Audio" && k !== "Subtitle")) throw new Error(`the media info lists ${kinds.join(", ")}: a placeholder is shown`);
    const menus = await playAndMenus(seen);
    same(menus.audio, [1, 4], "the audio menu");
    same(menus.subtitles, [5, 6, 7, 11], "the subtitle menu");
    await pickSubtitle(page, 11);
    await waitFor("track 11 to be answered", async () => seen.subtitles.find((r) => r.index === 11 && r.status === 200));
    await seek(page, 2);
    await waitFor("the cue of track 11", async () => (await activeText(page)) === "Babel track 11");
    noErrors(seen);
    await leave(page);

    await ok("PATCH", "/jellyscale/v1/track-preferences", JSON.stringify({ ShowAllTracks: true }), 200);
    await open();
    const all = await playAndMenus(seen);
    same(all.audio, [1, 2, 3, 4], "the audio menu with all tracks shown");
    same(all.subtitles, [5, 6, 7, 8, 9, 10, 11, 12, 13, 14], "the subtitle menu with all tracks shown");
    await leave(page);
  } finally {
    await apiCall(page, "PATCH", "/jellyscale/v1/track-preferences", JSON.stringify({ HideOtherAudio: false, HideOtherSubtitles: false, ShowAllTracks: false }));
    await restoreConfiguration(page, previous);
  }
});

// JS-121: a viewer whose audio language is German (AudioLanguagePreference deu, PlayDefaultAudioTrack off, which is what
// Jellyfin 12.4 needs to leave the file's flagged track: compat/captures/audio-default-12.4.md) starts a film in German.
// Babel Film's audio is 1 eng (the default, a 440 Hz tone), 2 deu (550 Hz), 3 fra, 4 untagged; Kino Film's is 1 deu (the
// default, 440 Hz), 2 eng (550 Hz); Mono Film has one English track (440 Hz). The tone each track has says which one the
// browser's speakers get, so the check reads the frequency of the sound the video element plays. Nothing before these
// scenarios plays Babel Film's second track, so the first play makes its segments.
const audioTone = (page) => page.evaluate(async () => {
  const video = document.querySelector("video");
  const context = new AudioContext();
  await context.resume();
  const analyser = context.createAnalyser();
  analyser.fftSize = 16384;
  context.createMediaElementSource(video).connect(analyser);
  analyser.connect(context.destination);
  let strongest = 0;
  for (let round = 0; round < 8; round++) { // The strongest bin over a few reads: a read at a moment of silence is no answer.
    await new Promise((resolve) => setTimeout(resolve, 400));
    const bins = new Float32Array(analyser.frequencyBinCount);
    analyser.getFloatFrequencyData(bins);
    let top = 0;
    for (let i = 1; i < bins.length; i++) if (bins[i] > bins[top]) top = i;
    if (bins[top] > -60) strongest = Math.round((top * context.sampleRate) / analyser.fftSize);
    if (strongest) break;
  }
  return strongest;
});

// Starts a film from its card in the library list, as jellyfin-web does for the play button on a card: no details page, so
// the request names no audio track and PlaybackInfo's own default decides (compat/captures/audio-default-12.4.md).
async function startFromCard(page, title) {
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: "Languages" }).first().click();
  const card = page.locator(".card:visible", { hasText: title }).first();
  await card.waitFor({ timeout: 30000 });
  await card.hover();
  await card.locator(".cardOverlayFab-primary").click();
  await waitFor("video to start", async () => (await time(page)) > 0.5, 60000);
}

async function withGermanViewer(page, run) {
  const userId = await page.evaluate(() => window.ApiClient.getCurrentUserId());
  const post = (body) => apiCall(page, "POST", `/Users/${userId}/Configuration`, JSON.stringify(body));
  const previous = await savedConfiguration(page);
  const saved = await post({ ...previous, AudioLanguagePreference: "deu", PlayDefaultAudioTrack: false });
  if (saved.status !== 204) throw new Error(`saving the settings: ${saved.status} ${saved.text}`);
  try {
    await run();
  } finally {
    await restoreConfiguration(page, previous);
  }
}

// The stream runs the worker started since a count of the log's lines: a thumbnail is not one (it writes mjpeg).
const streamRuns = (since) => ffmpegLines().slice(since).filter((line) => /-c:v (copy|libx26[45]|libsvtav1)/.test(line));

await scenario("a viewer whose audio language is German hears the German track of a film whose default is English, with the video copied", async ({ page, seen }) => {
  await withGermanViewer(page, async () => {
    const lines = ffmpegLines().length;
    const jobs = jobCount();
    await startFromCard(page, "Babel Film");
    const info = seen.infos[0];
    if ("AudioStreamIndex" in info.request) throw new Error(`the film was not started from its card: ${JSON.stringify(info.request).slice(0, 200)}`);
    if (info.source.DefaultAudioStreamIndex !== 2) throw new Error(`DefaultAudioStreamIndex ${info.source.DefaultAudioStreamIndex}, want the German track 2`);
    const url = new URL(info.source.TranscodingUrl ?? "http://none/", base);
    if (info.source.SupportsDirectPlay || url.searchParams.get("VideoCodec") !== "copy" || url.searchParams.get("AudioStreamIndex") !== "2") {
      throw new Error(`the reply is not a copy of track 2: direct play ${info.source.SupportsDirectPlay}, ${info.source.TranscodingUrl}`);
    }
    await advances(page, 2);
    const runs = await waitFor("the stream's ffmpeg", async () => (streamRuns(lines).length ? streamRuns(lines) : null), 30000);
    if (runs.length !== 1 || !/-c:v copy/.test(runs[0]) || /libx26[45]|libsvtav1/.test(runs[0])) throw new Error(`the video is not copied by one ffmpeg: ${runs.map((r) => r.slice(0, 400))}`);
    if (jobCount() !== jobs + 1) throw new Error(`${jobCount() - jobs} specs started, want 1`);
    // The sound is the German track's 550 Hz tone, not the English 440 Hz one.
    const tone = await audioTone(page);
    if (Math.abs(tone - 550) > 15) throw new Error(`the sound has its strongest tone at ${tone} Hz, want 550 (German); English is 440`);
    // The player's menu shows it selected: the check of a menu entry is hidden except on the chosen track.
    await page.mouse.move(300, 300);
    await page.mouse.move(600, 400);
    await waitFor("the audio button", async () => page.click(".btnAudio", { timeout: 1500 }).then(() => true, () => false), 30000);
    await page.waitForSelector(".actionSheetMenuItem", { timeout: 10000 });
    const selected = await page.evaluate(() => [...document.querySelectorAll(".actionSheetMenuItem")].filter((e) => e.querySelector(".check").style.visibility !== "hidden").map((e) => e.dataset.id));
    if (selected.join() !== "2") throw new Error(`the audio menu shows ${selected.join() || "nothing"} selected, want 2`);
    // Closed as menuIds closes one, and waited for: a click beside it does not always close it, and then leave()'s back
    // closed the menu instead of the player, which never sent Stopped (merge queue, run 44040147233). Each try clicks again.
    await waitFor("the audio menu to close", async () => {
      await page.mouse.click(5, 5);
      return (await page.locator(".actionSheetMenuItem").count()) === 0;
    }, 10000);
    seen.errors = seen.errors.filter((e) => !/ActionSheet closed without resolving/.test(e));
    noErrors(seen);
    await leave(page);
  });
});

// The same viewer plays directly, as before, where the film's own track is the German one or the only one: no job, no ffmpeg.
const playsDirectly = (title) => async ({ page, seen }) => {
  await withGermanViewer(page, async () => {
    const lines = ffmpegLines().length;
    const jobs = jobCount();
    await startFromCard(page, title);
    const info = seen.infos[0];
    if (info.source.DefaultAudioStreamIndex !== 1 || info.source.TranscodingUrl || !info.source.SupportsDirectPlay) {
      throw new Error(`default ${info.source.DefaultAudioStreamIndex}, direct play ${info.source.SupportsDirectPlay}, ${info.source.TranscodingUrl}: want track 1 played as it is`);
    }
    await advances(page, 2);
    const heard = await audioTone(page); // Track 1 is the 440 Hz tone in both films.
    if (Math.abs(heard - 440) > 15) throw new Error(`the sound has its strongest tone at ${heard} Hz, want 440`);
    if (jobCount() !== jobs || streamRuns(lines).length) throw new Error(`a job started: ${jobCount() - jobs} specs, ${streamRuns(lines).map((r) => r.slice(0, 200))}`);
    noErrors(seen);
    await leave(page);
  });
};
await scenario("a viewer whose audio language is German plays a film with only an English track directly, starting no job", playsDirectly("Mono Film"));
await scenario("a viewer whose audio language is German plays a film whose default track is German directly, starting no job", playsDirectly("Kino Film"));

// The usual way in: the details page and its Play button. jellyfin-web fills the page's Audio menu from the item, selects
// the item's DefaultAudioStreamIndex and sends the menu's value as AudioStreamIndex (compat/captures/audio-default-12.4.md,
// "the item"), so the viewer's language must already be in the item. It plays Babel Film, which the card scenario above
// has played in the same spec (German, copied): this scenario asserts the request, the reply and the sound, and counts no
// ffmpeg, because the stored segments are reused.
await scenario("a viewer whose audio language is German opens a film's details page, sees the German track selected in its Audio menu, and Play starts it, copied", async ({ page, seen }) => {
  await withGermanViewer(page, async () => {
    await page.goto(`${base}/web/`);
    await page.waitForSelector(".card", { timeout: 30000 });
    await page.locator(".card:visible", { hasText: "Languages" }).first().click();
    await page.locator(".cardText a:visible", { hasText: "Babel Film" }).first().click();
    await page.waitForSelector(".btnPlay:visible", { timeout: 30000 });
    await page.waitForSelector(".selectAudio", { state: "attached", timeout: 30000 });
    const menu = await page.evaluate(() => {
      const select = document.querySelector(".selectAudio");
      return { options: [...select.options].map((o) => o.value), selected: select.value };
    });
    if (menu.selected !== "2") throw new Error(`the details page's Audio menu has ${menu.selected} selected among ${menu.options}, want the German track 2`);
    await page.locator(".btnPlay:visible").first().click();
    await waitFor("video to start", async () => (await time(page)) > 0.5, 60000);
    const info = seen.infos[0];
    if (String(info.request.AudioStreamIndex) !== "2") throw new Error(`Play asked for AudioStreamIndex ${info.request.AudioStreamIndex}, want 2`);
    const url = new URL(info.source.TranscodingUrl ?? "http://none/", base);
    if (info.source.DefaultAudioStreamIndex !== 2 || info.source.SupportsDirectPlay || url.searchParams.get("VideoCodec") !== "copy" || url.searchParams.get("AudioStreamIndex") !== "2") {
      throw new Error(`the reply is not a copy of track 2: default ${info.source.DefaultAudioStreamIndex}, direct play ${info.source.SupportsDirectPlay}, ${info.source.TranscodingUrl}`);
    }
    await advances(page, 2);
    const tone = await audioTone(page);
    if (Math.abs(tone - 550) > 15) throw new Error(`the sound has its strongest tone at ${tone} Hz, want 550 (German); English is 440`);
    noErrors(seen);
    await leave(page);
  });
});

// A film whose own default is the viewer's language plays directly from the details page too, and a viewer with no
// language saved sees the file's default selected.
await scenario("from the details page a German viewer plays a film whose default is German directly, and a viewer with no language saved sees Babel Film's English track selected", async ({ page, seen }) => {
  const open = async (title) => {
    await page.goto(`${base}/web/`);
    await page.waitForSelector(".card", { timeout: 30000 });
    await page.locator(".card:visible", { hasText: "Languages" }).first().click();
    await page.locator(".cardText a:visible", { hasText: title }).first().click();
    await page.waitForSelector(".btnPlay:visible", { timeout: 30000 });
    await page.waitForSelector(".selectAudio", { state: "attached", timeout: 30000 });
    return page.evaluate(() => document.querySelector(".selectAudio").value);
  };
  const none = await open("Babel Film");
  if (none !== "1") throw new Error(`with no language saved the Audio menu has ${none} selected, want the file's default 1`);
  await withGermanViewer(page, async () => {
    const lines = ffmpegLines().length;
    const jobs = jobCount();
    const selected = await open("Kino Film");
    if (selected !== "1") throw new Error(`Kino Film's Audio menu has ${selected} selected, want its German default 1`);
    await page.locator(".btnPlay:visible").first().click();
    await waitFor("video to start", async () => (await time(page)) > 0.5, 60000);
    const info = seen.infos[0];
    if (!info.source.SupportsDirectPlay || info.source.TranscodingUrl || info.source.DefaultAudioStreamIndex !== 1) {
      throw new Error(`default ${info.source.DefaultAudioStreamIndex}, direct play ${info.source.SupportsDirectPlay}: want track 1 played as it is`);
    }
    if (jobCount() !== jobs || streamRuns(lines).length) throw new Error("a job started");
    noErrors(seen);
    await leave(page);
  });
});

// JS-041: the details page starts the film on the subtitle the viewer's mode and language choose. Trio Film's text subtitles
// are 2 fra (the first), 3 deu (flagged default) and 4 eng (test/fixtures/trio.sh), each with one cue, "Trio track N", from 1 to
// 5 seconds, and its audio is one English track that plays as it is. jellyfin-web fills its Subtitles menu from the item,
// selects the item's DefaultSubtitleStreamIndex (Off is -1) and sends the menu's value as SubtitleStreamIndex when Play is
// pressed (compat/captures/subtitles-default-12.4.md), so the viewer's mode must already be in the item. Nothing before these
// scenarios plays Trio Film, so its text tracks are read for the first time here.
async function withSubtitleMode(page, settings, run) {
  const userId = await page.evaluate(() => window.ApiClient.getCurrentUserId());
  const post = (body) => apiCall(page, "POST", `/Users/${userId}/Configuration`, JSON.stringify(body));
  const previous = await savedConfiguration(page);
  const saved = await post({ ...previous, ...settings });
  if (saved.status !== 204) throw new Error(`saving the settings: ${saved.status} ${saved.text}`);
  try {
    await run();
  } finally {
    await restoreConfiguration(page, previous);
  }
}

async function openTrio(page) {
  await page.goto("about:blank"); // A full load: the item is asked for again, as a viewer coming back to the page does.
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: "Languages" }).first().click();
  await page.locator(".cardText a:visible", { hasText: "Trio Film" }).first().click();
  await page.waitForSelector(".btnPlay:visible", { timeout: 30000 });
  await page.waitForSelector(".selectSubtitles", { state: "attached", timeout: 30000 });
  await page.waitForTimeout(500);
  return page.evaluate(() => {
    const select = document.querySelector(".selectSubtitles");
    return { options: [...select.options].map((o) => ({ value: o.value, label: o.textContent })), selected: select.value };
  });
}

await scenario("a viewer with mode Always and English opens Trio Film's details page, sees the English track selected in the Subtitles menu, and Play shows its cue", async ({ page, seen }) => {
  await withSubtitleMode(page, { SubtitleMode: "Always", SubtitleLanguagePreference: "eng" }, async () => {
    const menu = await openTrio(page);
    // jellyfin-web puts the track flagged default first, as it does in the Audio menu; the order is its own, so the entries are compared sorted.
    const values = menu.options.map((o) => Number(o.value)).sort((a, b) => a - b).join();
    if (values !== "-1,2,3,4") throw new Error(`the Subtitles menu lists ${values}, want Off and the film's three tracks -1,2,3,4`);
    if (menu.selected !== "4") throw new Error(`the Subtitles menu has ${menu.selected} selected (${JSON.stringify(menu.options)}), want the English track 4, not the first (2) or the flagged one (3)`);
    await page.locator(".btnPlay:visible").first().click();
    await waitFor("video to start", async () => (await time(page)) > 0.5, 60000);
    const info = seen.infos[0];
    if (String(info.request.SubtitleStreamIndex) !== "4") throw new Error(`Play asked for SubtitleStreamIndex ${info.request.SubtitleStreamIndex}, want 4`);
    if (info.source.DefaultSubtitleStreamIndex !== 4) throw new Error(`the reply's DefaultSubtitleStreamIndex is ${info.source.DefaultSubtitleStreamIndex}, want 4`);
    await seek(page, 2);
    await waitFor("the English track's cue", async () => (await activeText(page)) === "Trio track 4");
    noErrors(seen);
    await leave(page);
  });
});

await scenario("a viewer with mode None opens Trio Film's details page, sees Off selected in the Subtitles menu, and Play shows no cue", async ({ page, seen }) => {
  await withSubtitleMode(page, { SubtitleMode: "None", SubtitleLanguagePreference: "eng" }, async () => {
    const menu = await openTrio(page);
    if (menu.selected !== "-1") throw new Error(`the Subtitles menu has ${menu.selected} selected (${JSON.stringify(menu.options)}), want Off (-1)`);
    await page.locator(".btnPlay:visible").first().click();
    await waitFor("video to start", async () => (await time(page)) > 0.5, 60000);
    const info = seen.infos[0];
    if (String(info.request.SubtitleStreamIndex) !== "-1") throw new Error(`Play asked for SubtitleStreamIndex ${info.request.SubtitleStreamIndex}, want -1`);
    await seek(page, 2);
    // Every track's cue is on from 1 to 5 seconds: through them, with a second to spare, nothing is shown.
    const shown = [];
    for (let i = 0; i < 6; i++) {
      shown.push(await activeText(page));
      await page.waitForTimeout(500);
    }
    if (shown.some(Boolean)) throw new Error(`a cue is on screen with mode None: ${JSON.stringify(shown)}`);
    if ((await time(page)) < 3.5) throw new Error("the film did not play on through the cue window");
    noErrors(seen);
    await leave(page);
  });
});

// JS-147 (D-018): the details page's Audio menu lists every audio track the viewer has not hidden, as Jellyfin 12.4 does
// (compat/captures/audio-menu-12.4.md, which this reads the entries, their order and their labels from: the menu is
// jellyfin-web's own, sorted with the default first). Babel Film's menu is 1, 2, 3, 4 with 1 selected for a viewer with no
// language and 2 for a German one; the viewer who hides the other languages (JS-119) has 1 and 4, and 1, 2, 3 and 4 again
// once "show all tracks" is set and the page loaded again; Mono Film's has its one entry.
const capturedMenu = (film, language) => JSON.parse(readFileSync(`${process.env.E2E_CAPTURES}/audio-menu-12.4.json`, "utf8")).web.menus.find((m) => m.film === film && m.AudioLanguagePreference === language);

async function openDetails(page, title) {
  await page.goto("about:blank"); // A full load: the item is asked for again, as a viewer coming back to the page does.
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: "Languages" }).first().click();
  await page.locator(".cardText a:visible", { hasText: title }).first().click();
  await page.waitForSelector(".btnPlay:visible", { timeout: 30000 });
  await page.waitForSelector(".selectAudio", { state: "attached", timeout: 30000 });
  await page.waitForTimeout(500);
}

// The details page's Audio menu as the viewer sees it: its entries in order, the selected one, whether it is shown and enabled.
const audioMenu = (page) => page.evaluate(() => {
  const select = document.querySelector(".selectAudio");
  return {
    shown: !document.querySelector(".selectAudioContainer").classList.contains("hide"), enabled: !select.disabled,
    options: [...select.options].map((o) => ({ value: o.value, label: o.textContent })), selected: select.value,
  };
});

const sameEntries = (got, want, what) => {
  if (JSON.stringify(got) !== JSON.stringify(want)) throw new Error(`${what}: ${JSON.stringify(got)}, want ${JSON.stringify(want)}`);
};

await scenario("the details page's Audio menu lists every audio track the viewer has not hidden, in Jellyfin's order and labels, with the viewer's own selected", async ({ page }) => {
  const userId = await page.evaluate(() => window.ApiClient.getCurrentUserId());
  const ok = async (method, path, body) => {
    const answer = await apiCall(page, method, path, body);
    if (![200, 204].includes(answer.status)) throw new Error(`${method} ${path}: ${answer.status} ${answer.text}`);
  };
  const babel = capturedMenu("Menu Babel", "").options; // The same tracks as Babel Film's, so the same labels (test/fixtures/babel.sh).
  const previous = await savedConfiguration(page);
  try {
    await openDetails(page, "Babel Film");
    const none = await audioMenu(page);
    sameEntries(none.options, babel, "Babel Film's Audio menu for a viewer with no language saved");
    if (!none.shown || !none.enabled || none.selected !== "1") throw new Error(`no language saved: shown ${none.shown}, enabled ${none.enabled}, selected ${none.selected}, want shown, enabled and 1`);
    if (new Set(none.options.map((o) => o.label)).size !== none.options.length) throw new Error(`two entries read alike: ${JSON.stringify(none.options)}`);

    await ok("POST", `/Users/${userId}/Configuration`, JSON.stringify({ AudioLanguagePreference: "deu", PlayDefaultAudioTrack: false }));
    await openDetails(page, "Babel Film");
    const german = await audioMenu(page);
    sameEntries(german.options, babel, "Babel Film's Audio menu for a German viewer");
    if (german.selected !== "2") throw new Error(`German viewer: selected ${german.selected}, want 2`);

    await ok("POST", `/Users/${userId}/Configuration`, JSON.stringify({ AudioLanguagePreference: "eng", SubtitleLanguagePreference: "eng" }));
    await ok("PATCH", "/jellyscale/v1/track-preferences", JSON.stringify({ HideOtherAudio: true, HideOtherSubtitles: true, ShowAllTracks: false }));
    await openDetails(page, "Babel Film");
    const hidden = await audioMenu(page);
    sameEntries(hidden.options, [babel[0], babel[3]], "Babel Film's Audio menu for a viewer who hides the other languages");
    if (hidden.selected !== "1") throw new Error(`hiding viewer: selected ${hidden.selected}, want 1`);

    await ok("PATCH", "/jellyscale/v1/track-preferences", JSON.stringify({ ShowAllTracks: true }));
    await openDetails(page, "Babel Film");
    sameEntries((await audioMenu(page)).options, babel, "Babel Film's Audio menu with every track shown");

    await openDetails(page, "Mono Film");
    const mono = await audioMenu(page);
    if (mono.options.map((o) => o.value).join() !== "1" || !mono.shown || mono.enabled) throw new Error(`Mono Film's menu: ${JSON.stringify(mono)}, want its one entry, 1`);
  } finally {
    await apiCall(page, "PATCH", "/jellyscale/v1/track-preferences", JSON.stringify({ HideOtherAudio: false, HideOtherSubtitles: false, ShowAllTracks: false }));
    await restoreConfiguration(page, previous);
  }
});

// The German viewer picks an entry that is neither the selected one (2) nor the file's default (1): French, track 3, which
// the capture shows Jellyfin 12.4 sends as AudioStreamIndex "3", answers with DefaultAudioStreamIndex 3 and plays as 3's tone.
// jellyscale copies the video instead of re-encoding it (invariant 45). No earlier scenario plays Babel Film's third track,
// so the first play of it makes its segments: this scenario counts the ffmpeg runs (run it in its whole gate).
await scenario("a German viewer picks French in the details page's Audio menu and presses Play: the video is copied, track 3's tone is heard and the player's menu shows 3", async ({ page, seen }) => {
  await withGermanViewer(page, async () => {
    const lines = ffmpegLines().length;
    const jobs = jobCount();
    await openDetails(page, "Babel Film");
    if (!(await audioMenu(page)).options.some((o) => o.value === "3")) throw new Error(`the Audio menu has no entry 3: ${JSON.stringify((await audioMenu(page)).options)}`);
    await page.selectOption(".selectAudio", "3");
    await page.locator(".btnPlay:visible").first().click();
    await waitFor("video to start", async () => (await time(page)) > 0.5, 60000);
    const info = seen.infos[0];
    if (String(info.request.AudioStreamIndex) !== "3") throw new Error(`Play asked for AudioStreamIndex ${info.request.AudioStreamIndex}, want 3`);
    const url = new URL(info.source.TranscodingUrl ?? "http://none/", base);
    if (info.source.DefaultAudioStreamIndex !== 3 || info.source.SupportsDirectPlay || url.searchParams.get("VideoCodec") !== "copy" || url.searchParams.get("AudioStreamIndex") !== "3") {
      throw new Error(`the reply is not a copy of track 3: default ${info.source.DefaultAudioStreamIndex}, direct play ${info.source.SupportsDirectPlay}, ${info.source.TranscodingUrl}`);
    }
    await advances(page, 2);
    const runs = await waitFor("the stream's ffmpeg", async () => (streamRuns(lines).length ? streamRuns(lines) : null), 30000);
    if (runs.length !== 1 || !/-c:v copy/.test(runs[0]) || /libx26[45]|libsvtav1/.test(runs[0])) throw new Error(`the video is not copied by one ffmpeg: ${runs.map((r) => r.slice(0, 400))}`);
    if (jobCount() !== jobs + 1) throw new Error(`${jobCount() - jobs} specs started, want 1`);
    const tone = await audioTone(page);
    if (Math.abs(tone - 660) > 15) throw new Error(`the sound has its strongest tone at ${tone} Hz, want 660 (track 3); 440 is English and 550 German`);
    await page.mouse.move(300, 300);
    await page.mouse.move(600, 400);
    await waitFor("the audio button", async () => page.click(".btnAudio", { timeout: 1500 }).then(() => true, () => false), 30000);
    await page.waitForSelector(".actionSheetMenuItem", { timeout: 10000 });
    const selected = await page.evaluate(() => [...document.querySelectorAll(".actionSheetMenuItem")].filter((e) => e.querySelector(".check").style.visibility !== "hidden").map((e) => e.dataset.id));
    if (selected.join() !== "3") throw new Error(`the audio menu shows ${selected.join() || "nothing"} selected, want 3`);
    await waitFor("the audio menu to close", async () => {
      await page.mouse.click(5, 5);
      return (await page.locator(".actionSheetMenuItem").count()) === 0;
    }, 10000);
    seen.errors = seen.errors.filter((e) => !/ActionSheet closed without resolving/.test(e));
    noErrors(seen);
    await leave(page);
  });
});

// The same viewer picks English, the file's default (1), which is not the selected entry: it plays directly, track 1's
// tone, and starts no job and no ffmpeg.
await scenario("a German viewer picks English, the file's default, in the details page's Audio menu and presses Play: it plays directly with no job", async ({ page, seen }) => {
  await withGermanViewer(page, async () => {
    const lines = ffmpegLines().length;
    const jobs = jobCount();
    await openDetails(page, "Babel Film");
    if (!(await audioMenu(page)).options.some((o) => o.value === "1")) throw new Error(`the Audio menu has no entry 1: ${JSON.stringify((await audioMenu(page)).options)}`);
    await page.selectOption(".selectAudio", "1");
    await page.locator(".btnPlay:visible").first().click();
    await waitFor("video to start", async () => (await time(page)) > 0.5, 60000);
    const info = seen.infos[0];
    if (String(info.request.AudioStreamIndex) !== "1" || info.source.DefaultAudioStreamIndex !== 1 || !info.source.SupportsDirectPlay || info.source.TranscodingUrl) {
      throw new Error(`asked for ${info.request.AudioStreamIndex}, default ${info.source.DefaultAudioStreamIndex}, direct play ${info.source.SupportsDirectPlay}, ${info.source.TranscodingUrl}: want track 1 played as it is`);
    }
    await advances(page, 2);
    const heard = await audioTone(page);
    if (Math.abs(heard - 440) > 15) throw new Error(`the sound has its strongest tone at ${heard} Hz, want 440`);
    if (jobCount() !== jobs || streamRuns(lines).length) throw new Error(`a job started: ${jobCount() - jobs} specs, ${streamRuns(lines).map((r) => r.slice(0, 200))}`);
    noErrors(seen);
    await leave(page);
  });
});

// JS-147 (D-029): a pick the server cannot deliver right now (no worker has the cores free, or the transcode of that very
// spec just failed) is never played as another track: it ends in jellyfin-web's "Playback Error" dialog ("cannot be played at
// this time"), which the server answers at PlaybackInfo, and the client then stops asking. The viewer's saved language is not
// a pick: where its copy cannot start the file's default plays, directly. The item still lists every track in these states
// (the worker is there, so the menu is that of the healthy server).
// Busy is a job that holds 1000 cores on the live worker (a row the worker does not own is not counted, so it is the
// worker's); the worker's heartbeat does not touch it. A failed spec is made by playing it once, then marking its job failed:
// a failure is remembered for 30 s (failureMemory), so the replay follows at once. Neither is a state the stack can reach by
// load: a worker whose cores are taken needs more films than the stack plays.
const tooBusy = () => sql(`INSERT INTO transcode_jobs (spec_hash, spec, owner, start_segment, next_segment, state, worker, lease_expires_at, output_version, cost)
  SELECT 'e2e-busy', '{}', 'e2e-busy', 0, 0, 'running', id, now() + interval '1 hour', output_version, 1000 FROM transcode_nodes
  WHERE roles ~ '(^|,)worker(,|$)' ORDER BY seen_at DESC LIMIT 1`);

// Picks entry 'pick' in Babel Film's details page and presses Play, and waits for the dialog. Nothing is played: no stream
// was offered by any PlaybackInfo, no HLS request was made, and the client asked a few times and then stopped.
async function pickRefused(page, seen, pick) {
  await openDetails(page, "Babel Film");
  await page.selectOption(".selectAudio", pick);
  const asked = seen.infos.length, streams = seen.hls.length;
  await page.locator(".btnPlay:visible").first().click();
  await page.getByText("Playback Error").first().waitFor({ timeout: 20000 }); // A hang bound: the answer is at PlaybackInfo.
  await new Promise((resolve) => setTimeout(resolve, 5000));
  const infos = seen.infos.slice(asked);
  if (infos.length > 3) throw new Error(`the client asked PlaybackInfo ${infos.length} times`);
  for (const info of infos) {
    if (String(info.request.AudioStreamIndex) !== pick) throw new Error(`the page asked for AudioStreamIndex ${info.request.AudioStreamIndex}, want ${pick}`);
    if (info.source) throw new Error(`the server offered a plan for a pick it cannot deliver: ${JSON.stringify(info.source).slice(0, 300)}`);
  }
  if (seen.hls.length !== streams) throw new Error(`${seen.hls.length - streams} stream requests were made for a pick that is refused`);
}

await scenario("no worker has the cores free: a German viewer's pick of French ends in the dialog and plays no other track, and the saved language plays the file's default directly", async ({ page, seen }) => {
  await withGermanViewer(page, async () => {
    tooBusy();
    try {
      await openDetails(page, "Babel Film");
      const menu = await audioMenu(page);
      if (menu.options.map((o) => o.value).join() !== "1,2,3,4" || menu.selected !== "2") throw new Error(`the Audio menu in a busy server: ${JSON.stringify(menu)}, want 1 to 4 with 2 selected`);
      await pickRefused(page, seen, "3");
      // The saved language (2) is the menu's own selection: Play sends it, and it falls back to the file's default.
      await openDetails(page, "Babel Film");
      const asked = seen.infos.length;
      await page.locator(".btnPlay:visible").first().click();
      await waitFor("video to start", async () => (await time(page)) > 0.5, 60000);
      const info = seen.infos[asked];
      if (String(info.request.AudioStreamIndex) !== "2") throw new Error(`Play asked for AudioStreamIndex ${info.request.AudioStreamIndex}, want the saved language's 2`);
      if (!info.source?.SupportsDirectPlay || info.source.TranscodingUrl || info.source.DefaultAudioStreamIndex !== 1) {
        throw new Error(`default ${info.source?.DefaultAudioStreamIndex}, direct play ${info.source?.SupportsDirectPlay}, ${info.source?.TranscodingUrl}: want track 1 played as it is`);
      }
      await advances(page, 2);
      const heard = await audioTone(page);
      if (Math.abs(heard - 440) > 15) throw new Error(`the sound has its strongest tone at ${heard} Hz, want 440 (the file's default)`);
      await leave(page);
    } finally {
      sql("DELETE FROM transcode_jobs WHERE spec_hash = 'e2e-busy'");
    }
  });
});

await scenario("the transcode of a German viewer's pick of the untagged track just failed: asking again ends in the dialog and plays no other track", async ({ page, seen }) => {
  await withGermanViewer(page, async () => {
    // Track 4 is played once, so that the spec exists and PlaybackInfo offers it (the baseline the refusal is told from).
    await openDetails(page, "Babel Film");
    await page.selectOption(".selectAudio", "4");
    const asked = seen.infos.length;
    await page.locator(".btnPlay:visible").first().click();
    await waitFor("video to start", async () => (await time(page)) > 0.5, 60000);
    const url = new URL(seen.infos[asked].source?.TranscodingUrl ?? "http://none/", base);
    if (url.searchParams.get("AudioStreamIndex") !== "4") throw new Error(`the healthy server did not offer track 4: ${seen.infos[asked].source?.TranscodingUrl}`);
    await leave(page);
    await waitFor("the spec's job to end", async () => sql("SELECT count(*) FROM transcode_jobs WHERE state IN ('queued', 'running')") === "0", 30000);
    sql(`UPDATE transcode_jobs SET state = 'failed', error = 'ffmpeg exited 1', ended_at = now(), again = false
      WHERE spec_hash = (SELECT spec_hash FROM transcode_jobs WHERE NOT helper ORDER BY id DESC LIMIT 1)`);
    await pickRefused(page, seen, "4");
  });
});

// JS-089. A burnt PGS track is stored by a worker once the play asks for it,
// and a seek into a cue that began 60 s earlier is then a run that draws the cue
// from the stored track, so it is on screen at the first frame played after the
// seek and until it ends. The film's one cue is from 20 to 110 s; the player is
// at 12 s when the track is picked, and seeks to 80 s once the asset store holds
// the track (found without naming how it is stored: this film has no text track,
// so every object under subtitles/ of its library's assets is the image track).
// That run's ffmpeg has more inputs than a JS-071 run of the film has
// (the film for its video and again for its audio: two), which is the stored
// track as one more. The scenario is above runLater() so that it runs with the
// others, before the gate decides. No error is reported.
const storedObjects = () => (existsSync(`${process.env.E2E_ASSETS}/subtitles`) ? readdirSyncDeep(`${process.env.E2E_ASSETS}/subtitles`) : []);
function readdirSyncDeep(dir) {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? readdirSyncDeep(join(dir, e.name)) : [join(dir, e.name)]));
}
await scenario("a seek into a PGS cue 60 s old, once the track is stored, shows the cue at the first frame and the run drew from the stored track", async ({ page, seen }) => {
  const logged = ffmpegLines().length;
  await play(page, "Transcode", "Pgs Long Cue Film");
  await seek(page, 12);
  await chooseBurn(page, seen, 2);
  await waitFor("the image track to be stored by a worker", async () => storedObjects().length >= 1 && sql("SELECT count(*) FROM subtitle_jobs WHERE state IN ('queued', 'running')") === "0", 90000);
  await seek(page, 80);
  await waitFor("the first frames after the seek to 80 s", async () => (await time(page)) >= 80.1, 60000);
  expectBright(await brightThirds(page, true), "top");
  const runs = ffmpegLines().slice(logged).filter((line) => /overlay=eof_action/.test(line));
  const seeked = runs.filter((line) => Number(/-start_number (\d+)/.exec(line)?.[1]) >= 15);
  if (!seeked.length) throw new Error(`no run began near 80 s: ${runs.map((l) => /-start_number (\d+)/.exec(l)?.[1])}`);
  // The film has audio, so a JS-071 run already has two inputs; the track is a third.
  if (!seeked.some((line) => (line.match(/ -i /g) ?? []).length >= 3)) throw new Error(`the run that began near 80 s was not given the stored track as an input of its own: ${seeked.at(-1)}`);
  await waitFor("playback to 90 s with the cue still on", async () => (await time(page)) >= 90, 60000);
  expectBright(await brightThirds(page, true), "top");
  noErrors(seen);
  await leave(page);
});

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
