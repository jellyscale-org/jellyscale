// Phase 3 gate: unmodified jellyfin-web plays films Chrome can't direct-play,
// through the router with two api replicas, a stream replica and a worker on
// its own process, and the transcode ends when it should.
// Usage: E2E_SCRATCH=<worker scratch dir> node transcode.mjs <base_url> <user> <password>
// Run from a directory with playwright-core installed.
import { execFileSync, spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { createRequire } from "node:module";

const require = createRequire(`${process.cwd()}/`);
const { chromium } = require("playwright-core");

const [home, user, password] = process.argv.slice(2);
let base = home;
const scratch = process.env.E2E_SCRATCH;
const workerPid = Number(process.env.E2E_WORKER_PID);
if (!workerPid) throw new Error("the transcode gate needs E2E_WORKER_PID (test/e2e/run.sh sets it)");
if (!scratch) throw new Error("E2E_SCRATCH is the worker's scratch directory");

const browser = await chromium.launch({
  executablePath: process.env.CHROMIUM_PATH ?? "/usr/bin/chromium",
  args: ["--disable-gpu", "--autoplay-policy=no-user-gesture-required"],
});
const failures = [];

function sql(text) {
  return execFileSync("docker", ["exec", process.env.E2E_PG, "psql", "-U", "postgres", "-Atc", text]).toString().trim();
}

// The worker's ffmpeg processes, read from the process table.
function ffmpegRuns() {
  try {
    const lines = execFileSync("pgrep", ["-af", `ffmpeg.*${scratch}`]).toString().trim().split("\n").filter(Boolean);
    // A re-encode says its first segment; a copy says where it seeks to, the
    // keyframe before its first segment, from which the segment follows on
    // these films (a keyframe every two seconds, segments of four).
    return lines.map((line) => ({
      line,
      copy: /-c:v copy/.test(line),
      start: /-start_number (\d+)/.test(line) ? Number(/-start_number (\d+)/.exec(line)[1]) : Math.ceil(Number(/-ss ([\d.]+)/.exec(line)?.[1] ?? 0) / 4),
      audio: /-map 1:(\d+)/.exec(line)?.[1],
    }));
  } catch {
    return []; // pgrep exits 1 when nothing matches.
  }
}

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
  const seen = { infos: [], hls: [], deletes: [], errors: [], console: [] };
  page.on("pageerror", (error) => seen.errors.push(error.message));
  page.on("console", (message) => { if (/hls|playback/i.test(message.text())) seen.console.push(message.text().slice(0, 200)); });
  page.on("request", (request) => {
    if (request.method() === "DELETE" && /ActiveEncodings/i.test(request.url())) seen.deletes.push(request.url());
  });
  page.on("response", async (response) => {
    const url = new URL(response.url());
    if (!response.url().startsWith(base)) return;
    if (response.status() >= 500) seen.errors.push(`${response.status()} ${url.pathname}`);
    if (/^\/videos\/.*(m3u8|\/hls1\/)/i.test(url.pathname)) {
      seen.hls.push({ at: Date.now(), status: response.status(), routed: response.headers()["x-routed-to"], path: url.pathname, query: url.searchParams });
    }
    if (response.request().method() === "POST" && /\/PlaybackInfo$/i.test(url.pathname)) {
      const request = JSON.parse(response.request().postData() ?? "{}");
      const reply = await response.json().catch(() => null);
      if (reply) seen.infos.push({ request, source: reply.MediaSources[0], session: reply.PlaySessionId });
    }
  });
  await login(page);
  return { context, page, seen };
}

const time = (page) => page.evaluate(() => document.querySelector("video")?.currentTime ?? -1);
const duration = (page) => page.evaluate(() => document.querySelector("video")?.duration ?? 0);

async function waitFor(what, test, timeout = 30000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const value = await test();
    if (value) return value;
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  throw new Error(`timed out waiting for ${what}`);
}

async function play(page, library, title) {
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: library }).first().click();
  await page.locator(".cardText a:visible", { hasText: title }).first().click();
  await page.locator(".btnPlay:visible").first().click();
  await waitFor("video to start", async () => (await time(page)) > 0.5, 60000);
}

async function advances(page, seconds = 1.5) {
  const before = await time(page);
  await waitFor(`currentTime to pass ${before + seconds}`, async () => (await time(page)) > before + seconds, 40000);
}

function noErrors(seen) {
  if (seen.errors.length) throw new Error(`errors: ${seen.errors.join("; ")}`);
  const bad = seen.hls.filter((r) => r.status >= 400 && r.status !== 410);
  if (bad.length) throw new Error(`HLS requests failed: ${JSON.stringify(bad.slice(0, 3))}`);
}

const transcodeUrl = (seen) => seen.infos.map((i) => i.source.TranscodingUrl).filter(Boolean).at(-1);

// E2E_ONLY="audio" runs just the scenarios whose name matches. A name is a quoted
// literal, with any part known only at run time after ": ", so that test/report.sh
// can annotate the scenario at its own line.
async function scenario(name, run) {
  if (process.env.E2E_ONLY && !new RegExp(process.env.E2E_ONLY, "i").test(name)) return;
  const v = await viewer();
  let passed = false;
  try {
    await run(v);
    passed = true;
  } catch (error) {
    failures.push(`${name}: ${error.message.split("\n")[0]}`);
    console.log(`FAIL  ${name}: ${error.message.split("\n")[0]}`);
    await v.page.screenshot({ path: `${process.env.TMPDIR ?? "/tmp"}/jellyscale-e2e-transcode.png` }).catch(() => {});
  } finally {
    await v.context.close().catch(() => {});
  }
  // Nothing may be left running for the next scenario.
  await waitFor("ffmpeg to exit between scenarios", async () => ffmpegRuns().length === 0, 60000).catch(() => {
    failures.push(`${name}: ffmpeg still running afterwards`);
    console.log(`FAIL  ${name}: ffmpeg still running afterwards`);
    passed = false;
  });
  if (passed) console.log(`ok    ${name}`);
}

const leave = async (page) => {
  // The player reports its start without waiting for its page, and only that
  // page sends Stopped on leaving. It listens for leaving before it first shows
  // the position, so going back any sooner leaves the film playing.
  await page.waitForFunction(() => location.hash.startsWith("#/video") && document.querySelector(".osdPositionText")?.textContent, null, { timeout: 15000 });
  const stopped = page.waitForResponse((r) => /\/Sessions\/Playing\/Stopped/i.test(r.url()), { timeout: 15000 });
  await page.evaluate(() => window.history.back());
  const response = await stopped.catch(async () => {
    // jellyfin-web sends nothing if it thinks the stream is changing, or if its
    // player page never saw the start. This says how far the page got.
    const video = await page.evaluate(() => {
      const v = document.querySelector("video");
      return v ? `a video at ${v.currentTime} s, paused ${v.paused}` : "no video";
    });
    throw new Error(`no Stopped within 15 s of going back: the page is ${new URL(page.url()).hash} with ${video}`);
  });
  if (response.status() !== 204) throw new Error(`Stopped answered ${response.status()}`);
  await response.finished();
  const timing = response.request().timing();
  console.log(`      Stopped answered in ${Math.round(timing.responseEnd - timing.requestStart)} ms`);
};

// Each of these scenarios runs on a film whose video is copied and on one that
// has to be re-encoded (interlaced), with the same structure, so what the
// system does for a stream does not depend on how it was made.
const flavours = [
  { name: "copied", copy: true, far: "Zeta Film", audio: "Theta Film", resume: "Mu Film" },
  { name: "re-encoded", copy: false, far: "Zeta Interlaced", audio: "Theta Interlaced", resume: "Mu Interlaced" },
];

for (const flavour of flavours) await scenario("an AC3 film transcodes, plays from the stream replica, then far seek and stop end the right ffmpeg" + `: POSIX, ${flavour.name}`, async ({ page, seen }) => {
  await play(page, "Transcode", flavour.far);
  const source = seen.infos.at(-1).source;
  if (source.SupportsDirectPlay !== false || source.TranscodingSubProtocol !== "hls" || source.TranscodingContainer !== "mp4") {
    throw new Error(`PlaybackInfo did not ask for a transcode: ${JSON.stringify(source).slice(0, 300)}`);
  }
  if (!/TranscodeReasons=AudioCodecNotSupported/.test(source.TranscodingUrl)) throw new Error(`reasons: ${source.TranscodingUrl}`);
  if (/VideoCodec=copy/.test(source.TranscodingUrl) !== flavour.copy) throw new Error(`copy expected ${flavour.copy}: ${source.TranscodingUrl}`);
  if (!(await page.evaluate(() => document.querySelector("video").src.startsWith("blob:")))) throw new Error("not played through MSE");
  await advances(page);
  noErrors(seen);
  if (!seen.hls.some((r) => /master\.m3u8$/i.test(r.path)) || !seen.hls.some((r) => /\/hls1\/main\/0\.mp4$/i.test(r.path))) throw new Error("no master or segment 0");
  if (seen.hls.some((r) => r.routed !== "stream")) throw new Error("HLS was not served by the stream replica");

  // Far past the encoded point: a job starts there, and the first one stops.
  const first = ffmpegRuns();
  if (first.length !== 1) throw new Error(`expected one ffmpeg, saw ${first.length}`);
  if (first[0].copy !== flavour.copy || /libx264/.test(first[0].line) === flavour.copy) throw new Error(`ffmpeg does not do what ${flavour.name} means: ${first[0].line.slice(0, 400)}`);
  const total = await duration(page);
  const target = total * 0.8;
  const seeked = Date.now();
  await page.evaluate((t) => { document.querySelector("video").currentTime = t; }, target);
  const far = await waitFor("the first segment of the far seek", async () => seen.hls.find((r) => r.at > seeked && r.status === 200 && Number(r.path.match(/\/(\d+)\.mp4$/i)?.[1]) >= Math.floor(target / 4) - 1), 60000);
  console.log(`      far seek: first segment answered ${far.at - seeked} ms after the seek`);
  await waitFor("playback past the seek target", async () => (await time(page)) > target + 1.5, 60000);
  await waitFor("the replaced ffmpeg to exit and one to start at the seek", async () => {
    const runs = ffmpegRuns();
    return runs.length === 1 && runs[0].start >= Math.floor(target / 4) - 2;
  }, 20000).catch(async () => { throw new Error(`ffmpeg runs after the seek: ${JSON.stringify(ffmpegRuns().map((r) => r.start))}`); });
  noErrors(seen);

  await leave(page);
  await waitFor("ffmpeg to exit after Stopped", async () => ffmpegRuns().length === 0, 20000);
});

await scenario("S3: a film from the bucket transcodes and plays", async ({ page, seen }) => {
  await play(page, "Cloud", "Eta Film");
  if (seen.infos.at(-1).source.TranscodingUrl === undefined) throw new Error("no TranscodingUrl");
  await advances(page, 3);
  noErrors(seen);
  if (!seen.hls.some((r) => /\/hls1\/main\/\d+\.mp4$/i.test(r.path) && r.status === 200)) throw new Error("no segment was served");
  await leave(page);
});

// ---- Copied streams ----
// H.264 in AC3 films whose keyframes are at 0, 3, 7, 13, 18 ... (run.sh): the
// captured Jellyfin cut a copied MP4 at 6 s steps it then did not keep to. Ours
// cuts on the keyframes the file has, and says so exactly.
const keyframeLengths = [7, 6, 5, 8, 5, 8, 5, 8, 5, 8, 5, 8, 5, 7];
const frame = 1001 / 24000;

// ffprobe may stop reading before the end, which a pipe reports as an error, so
// it reads a file.
function probe(input, ...args) {
  const dir = mkdtempSync(`${tmpdir()}/jellyscale-e2e-`);
  try {
    writeFileSync(`${dir}/in.mp4`, input);
    return execFileSync("ffprobe", ["-v", "error", ...args, `${dir}/in.mp4`], { maxBuffer: 1 << 28 }).toString();
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

async function bytesOf(url) {
  const response = await fetch(url);
  if (!response.ok) throw new Error(`${response.status} for ${url.pathname}`);
  return Buffer.from(await response.arrayBuffer());
}

async function playlistOf(masterUrl) {
  const master = new URL(masterUrl, base);
  const mainUrl = new URL((await (await fetch(master)).text()).split("\n").find((line) => line.startsWith("main.m3u8")), master);
  const text = await (await fetch(mainUrl)).text();
  const lines = text.split("\n");
  const segments = [];
  lines.forEach((line, i) => {
    if (line.startsWith("#EXTINF:")) segments.push({ length: parseFloat(line.slice(8)), url: new URL(lines[i + 1], mainUrl) });
  });
  return { text, segments, init: new URL(/URI="([^"]+)"/.exec(text)[1], mainUrl), target: Number(/TARGETDURATION:(\d+)/.exec(text)[1]) };
}

// Where a segment's video starts and ends, in the stream's own clock.
function span(init, body) {
  const packets = probe(Buffer.concat([init, body]), "-select_streams", "v:0", "-show_entries", "packet=pts_time,duration_time,flags", "-of", "csv=p=0")
    .trim().split("\n").map((line) => line.split(","));
  const start = Math.min(...packets.filter((p) => p[2]?.startsWith("K")).map((p) => parseFloat(p[0])));
  const end = Math.max(...packets.map((p) => parseFloat(p[0]) + parseFloat(p[1])));
  return { start, end };
}

async function copiedFilm({ page, seen }, library, title, sourceFile, where, codecs = /^avc1\./) {
  await play(page, library, title);
  const source = seen.infos.at(-1).source;
  // The decision first: a re-encode would otherwise be reported as a playlist
  // naming the wrong codec.
  if (!/VideoCodec=copy/.test(source.TranscodingUrl) || !/TranscodeReasons=AudioCodecNotSupported/.test(source.TranscodingUrl)) throw new Error(`not a copy: ${source.TranscodingUrl}`);
  const answer = await fetch(new URL(source.TranscodingUrl, base));
  const master = await answer.text();
  if (!answer.ok) throw new Error(`the master playlist answered ${answer.status}: ${JSON.stringify(master)}`);
  if (!codecs.test(/CODECS="([^,"]+),/.exec(master)?.[1] ?? "")) throw new Error(`the master playlist names the wrong codec: ${JSON.stringify(master)}`);
  if (source.SupportsDirectPlay !== false || source.TranscodingSubProtocol !== "hls" || source.TranscodingContainer !== "mp4") throw new Error("the reply fields are not the capture's");
  const runs = ffmpegRuns();
  if (runs.length !== 1 || !runs[0].copy || /libx264/.test(runs[0].line)) throw new Error(`ffmpeg is not copying the video: ${runs.map((r) => r.line.slice(0, 300))}`);
  await advances(page, 3);
  noErrors(seen);

  // The playlist is the file's keyframes, which fall on frames (23.976 fps) near the times asked for.
  const playlist = await playlistOf(source.TranscodingUrl);
  const lengths = playlist.segments.map((x) => x.length);
  if (lengths.length !== keyframeLengths.length || lengths.some((x, i) => Math.abs(x - keyframeLengths[i]) > 2 * frame + 0.001)) {
    throw new Error(`playlist lengths ${lengths}`);
  }
  if (playlist.target !== Math.ceil(Math.max(...lengths)) || /DISCONTINUITY/.test(playlist.text)) throw new Error(`target ${playlist.target}, or a discontinuity`);

  // A far seek, with nothing stored there yet.
  const total = await duration(page);
  const target = total * 0.7;
  const seeked = Date.now();
  await page.evaluate((t) => { document.querySelector("video").currentTime = t; }, target);
  const first = await waitFor("the first segment of the far seek", async () => seen.hls.find((r) => r.at > seeked && r.status === 200 && /\/\d+\.mp4$/i.test(r.path)), 60000);
  await waitFor("playback past the seek target", async () => (await time(page)) > target + 1, 60000);
  console.log(`      ${where}, copied: far seek, first segment answered ${first.at - seeked} ms after the seek, playing after ${Date.now() - seeked} ms`);

  // Every segment, fetched as a player would, some from jobs that began in the
  // middle of the film: they must be the file's video, byte for byte, cut where
  // the playlist says.
  // As another viewer: one session's new request replaces its own earlier job,
  // and the player is still asking for what it needs.
  const downloader = (url) => { url.searchParams.set("PlaySessionId", randomBytes(16).toString("hex")); return url; };
  const init = await bytesOf(downloader(playlist.init));
  const bodies = [];
  for (const segment of playlist.segments) bodies.push(await bytesOf(downloader(segment.url)));
  const spans = bodies.map((body) => span(init, body));
  spans.forEach((x, i) => {
    const length = i + 1 < spans.length ? spans[i + 1].start - x.start : x.end - x.start;
    if (Math.abs(length - lengths[i]) > frame + 0.001) throw new Error(`segment ${i} is ${length.toFixed(3)} s long, the playlist says ${lengths[i]}`);
  });
  const dir = mkdtempSync(`${tmpdir()}/jellyscale-e2e-`);
  writeFileSync(`${dir}/all.mp4`, Buffer.concat([init, ...bodies]));
  const copied = execFileSync("ffmpeg", ["-v", "error", "-i", `${dir}/all.mp4`, "-map", "0:v:0", "-c", "copy", "-f", "md5", "-"]).toString().trim();
  rmSync(dir, { recursive: true, force: true });
  const original = execFileSync("ffmpeg", ["-v", "error", "-i", sourceFile, "-map", "0:v:0", "-c", "copy", "-f", "md5", "-"]).toString().trim();
  if (copied !== original) throw new Error(`the video is not the source's: ${copied} against ${original}`);
  const audio = probe(Buffer.concat([init, bodies[0]]), "-select_streams", "a:0", "-show_entries", "stream=codec_name", "-of", "csv=p=0").trim();
  if (audio !== "aac") throw new Error(`audio is ${audio}`);
  console.log(`      ${where}: ${bodies.length} segments cut as the playlist says, video identical to the file`);

  // Played through the seams, now that every segment exists: no stall, no error.
  await page.evaluate(() => { document.querySelector("video").currentTime = 0; });
  await waitFor("playback from the start", async () => (await time(page)) < 2 && (await time(page)) > 0.2, 30000);
  await page.evaluate(() => { window.__waits = 0; document.querySelector("video").addEventListener("waiting", () => window.__waits++); });
  await waitFor("playback to pass 15 s, over two seams", async () => (await time(page)) > 15, 40000);
  const health = await page.evaluate(() => {
    const video = document.querySelector("video");
    const quality = video.getVideoPlaybackQuality();
    return { waits: window.__waits, error: video.error?.message ?? null, corrupted: quality.corruptedVideoFrames };
  });
  if (health.waits || health.error || health.corrupted) throw new Error(`playback through the seams: ${JSON.stringify(health)}`);
  noErrors(seen);
  await leave(page);
}

const media = process.env.E2E_MEDIA;
await scenario("copy, POSIX, Matroska: an H.264 and AC3 film plays with the video copied", (v) => copiedFilm(v, "Transcode", "Rho Film", `${media}/Rho Film (2041)/Rho Film (2041).mkv`, "POSIX mkv"));
await scenario("copy, POSIX, MP4: the playlist is exact where Jellyfin's lies", (v) => copiedFilm(v, "Transcode", "Qoppa Film", `${media}/Qoppa Film (2042)/Qoppa Film (2042).mp4`, "POSIX mp4"));
await scenario("copy, S3, Matroska", (v) => copiedFilm(v, "Cloud", "Heta Film", `${media}/Rho Film (2041)/Rho Film (2041).mkv`, "S3 mkv"));
await scenario("copy, S3, MP4", (v) => copiedFilm(v, "Cloud", "Koppa Film", `${media}/Qoppa Film (2042)/Qoppa Film (2042).mp4`, "S3 mp4"));

// Chapters: the details page lists none while there are no chapter images, as in
// Jellyfin 12.4 (compat/captures/chapters-12.4.md), and the player knows them:
// its seek bar has a marker for each and its next and previous chapter buttons
// show. Next chapter seeks to the next one's start, which the transcodes' clock
// (the file's, 2 s late in these two) leaves where the file says.
// The chapters of these films, by start in seconds.
const chapters = [["Opening", 0], ["Middle Part", 20], ["Chapter 3", 45.5]];
// The video's position when the seek that `act` starts is done, before playing
// moves it on: a seek that lands 2 s off shows here, which polling for a window
// of several seconds would not see.
async function seekedAfter(page, act) {
  await page.evaluate(() => {
    window.seekedAt = null;
    document.querySelector("video").addEventListener("seeked", (e) => { window.seekedAt ??= e.target.currentTime; }, { once: true });
  });
  await act();
  await waitFor("the seek to finish", async () => await page.evaluate(() => window.seekedAt !== null), 60000);
  return await page.evaluate(() => window.seekedAt);
}
async function chapterSeek({ page, seen }, title, how) {
  const imageRequests = [];
  page.on("request", (r) => { if (/\/Images\/Chapter\//i.test(r.url())) imageRequests.push(r.url()); });
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: "Transcode" }).first().click();
  await page.locator(".cardText a:visible", { hasText: title }).first().click();
  await page.locator(".btnPlay:visible").first().waitFor({ timeout: 30000 });
  await page.waitForTimeout(1500);
  const section = await page.evaluate(() => document.querySelector("#scenesCollapsible")?.classList.contains("hide") ?? true);
  if (!section) throw new Error("the details page lists chapters without any picture to show");
  await page.locator(".btnPlay:visible").first().click();
  await waitFor("video to start", async () => (await time(page)) > 0.5, 60000);
  const source = seen.infos.at(-1).source;
  if (how === "direct" && (source.SupportsDirectPlay !== true || source.TranscodingUrl)) throw new Error(`not direct play: ${JSON.stringify(source).slice(0, 300)}`);
  if (how === "copy" && !/VideoCodec=copy/.test(source.TranscodingUrl ?? "")) throw new Error(`not a copy: ${source.TranscodingUrl}`);
  if (how === "encode" && !/VideoCodec=h264/i.test(source.TranscodingUrl ?? "")) throw new Error(`not re-encoded: ${source.TranscodingUrl}`);

  // What the player was given: the chapters, named as the file and Jellyfin do, without an image.
  const given = await page.evaluate(async () => {
    const client = window.ApiClient;
    const id = new URLSearchParams(location.hash.split("?")[1]).get("id") ?? (await client.getJSON(client.getUrl("Sessions"))).find((s) => s.NowPlayingItem)?.NowPlayingItem.Id;
    return (await client.getItem(client.getCurrentUserId(), id)).Chapters;
  });
  if (JSON.stringify(given.map((c) => [c.Name, c.StartPositionTicks / 1e7])) !== JSON.stringify(chapters)) throw new Error(`chapters: ${JSON.stringify(given)}`);
  if (given.some((c) => "ImageTag" in c)) throw new Error(`a chapter has an ImageTag: ${JSON.stringify(given)}`);
  await page.mouse.move(600, 400);
  const osd = await page.evaluate(() => ({
    markers: document.querySelectorAll(".sliderMarkerContainer .sliderMarker").length,
    next: !document.querySelector(".btnNextChapter")?.classList.contains("hide"),
    previous: !document.querySelector(".btnPreviousChapter")?.classList.contains("hide"),
  }));
  if (osd.markers !== chapters.length || !osd.next || !osd.previous) throw new Error(`the player's chapter controls: ${JSON.stringify(osd)}`);

  for (const [name, start] of chapters.slice(1)) {
    const seeked = Date.now();
    await page.mouse.move(600, 400);
    const landed = await seekedAfter(page, () => page.click(".btnNextChapter"));
    if (landed < start - 0.5 || landed > start + 1) throw new Error(`${name}: the seek landed at ${landed} s, not ${start} s`);
    if (how !== "direct") {
      // The segment asked for is the one at the chapter on the playlist's clock.
      const segment = await waitFor(`a segment at ${start} s`, async () => seen.hls.find((r) => r.at > seeked && r.status === 200 && /\/\d+\.mp4$/i.test(r.path)
        && Math.abs(Number(r.path.match(/\/(\d+)\.mp4$/i)[1]) - Math.floor(start / 4)) <= 1), 60000);
      console.log(`      ${name}: segment ${segment.path.match(/\/(\d+)\.mp4$/i)[1]} for ${start} s`);
    }
    await advances(page, 1.5);
  }
  // Ten seconds into the last chapter, previous goes back to the middle one.
  await page.mouse.move(600, 400);
  const back = await seekedAfter(page, () => page.click(".btnPreviousChapter"));
  if (back < 19.5 || back > 21) throw new Error(`previous chapter landed at ${back} s, not 20 s`);
  noErrors(seen);
  if (imageRequests.length) throw new Error(`the page asked for chapter images: ${imageRequests.join(" ")}`);
  await leave(page);
}
await scenario("chapters, direct play, Matroska: the player's chapter buttons seek to each chapter", (v) => chapterSeek(v, "Chapters Direct", "direct"));
await scenario("chapters, direct play, MP4: the player's chapter buttons seek to each chapter", (v) => chapterSeek(v, "Chapters Mp4", "direct"));
await scenario("chapters, transcode with the video copied and the stream starting late", (v) => chapterSeek(v, "Chapters Copy", "copy"));
await scenario("chapters, transcode with the video re-encoded and the stream starting late", (v) => chapterSeek(v, "Chapters Encode", "encode"));

// AV1 is decoded in software by Chrome, so CI plays it: direct play, and a copy
// through HLS, in either container and from S3, cut where the playlist says.
await scenario("copy, POSIX, Matroska: AV1 and AC3 plays with the video copied", (v) => copiedFilm(v, "Transcode", "Sampi Film", `${media}/Sampi Film (2080)/Sampi Film (2080).mkv`, "POSIX AV1 mkv", /^av01\.0\.\d\dM\.08$/));
await scenario("copy, S3, Matroska: AV1", (v) => copiedFilm(v, "Cloud", "Mem Film", `${media}/Sampi Film (2080)/Sampi Film (2080).mkv`, "S3 AV1 mkv", /^av01\.0\.\d\dM\.08$/));
await scenario("an AV1 film in MP4 that the client plays direct plays", async ({ page, seen }) => {
  await play(page, "Transcode", "Heth Film");
  const source = seen.infos.at(-1).source;
  if (source.SupportsDirectPlay !== true || source.TranscodingUrl) throw new Error(`not direct play: ${JSON.stringify(source)}`);
  if (ffmpegRuns().length) throw new Error("ffmpeg is running for a direct play");
  await advances(page, 2);
  await page.evaluate(() => { document.querySelector("video").currentTime = 60; });
  await waitFor("playback past 61 s", async () => (await time(page)) > 61, 40000);
  const shown = await page.evaluate(() => ({ frames: document.querySelector("video").getVideoPlaybackQuality().totalVideoFrames, width: document.querySelector("video").videoWidth }));
  if (shown.frames < 10 || shown.width !== 640) throw new Error(`the AV1 picture did not decode: ${JSON.stringify(shown)}`);
  noErrors(seen);
  await leave(page);
});
await scenario("copy, POSIX, MP4: AV1 resumes mid-film from a copy", async ({ page, seen }) => {
  const { firstFrameMs, starts } = await resume(page, "Transcode", "San Film", 40);
  const source = seen.infos.at(-1).source;
  if (!/VideoCodec=copy/.test(source.TranscodingUrl)) throw new Error(`not a copy: ${source.TranscodingUrl}`);
  console.log(`      AV1 copied from MP4: time to first frame on resume at 40 s: ${firstFrameMs} ms; ffmpeg started at ${starts}`);
  noErrors(seen);
  await leave(page);
});

// What Chrome plays decides HEVC, and CI's Chrome has no hardware decoder, so
// it sends no HEVC in its profile and the film is re-encoded to H.264, with the
// reasons Jellyfin gives (compat/captures/hevc-av1-12.4.md). A machine with one
// is run with E2E_HEVC=1, which says what to expect: the runner chooses, the
// test never asks the browser.
const hevcPlays = process.env.E2E_HEVC === "1";
await scenario("HEVC in MP4 with AAC audio" + `: ${hevcPlays ? "direct plays (E2E_HEVC=1)" : "re-encoded to H.264"}`, async ({ page, seen }) => {
  await play(page, "Transcode", "Yot Film");
  const source = seen.infos.at(-1).source;
  console.log("      Chrome says: " + JSON.stringify(await page.evaluate(() => ({
    hevc: MediaSource.isTypeSupported('video/mp4; codecs="hvc1.1.6.L120.B0"'), av1: MediaSource.isTypeSupported('video/mp4; codecs="av01.0.08M.08"'),
  }))));
  if (hevcPlays) {
    if (source.SupportsDirectPlay !== true || source.TranscodingUrl) throw new Error(`not direct play: ${JSON.stringify(source)}`);
  } else {
    const query = new URL(source.TranscodingUrl, base).searchParams;
    if (query.get("VideoCodec") !== "h264" || query.get("TranscodeReasons") !== "VideoCodecNotSupported") throw new Error(`not a re-encode for the video codec: ${source.TranscodingUrl}`);
  }
  await advances(page, 2);
  noErrors(seen);
  await leave(page);
});
await scenario("HEVC in Matroska with AC3 audio" + `: ${hevcPlays ? "copied (E2E_HEVC=1)" : "re-encoded to H.264"}`, async ({ page, seen }) => {
  await play(page, "Transcode", "Lamed Film");
  const source = seen.infos.at(-1).source;
  const query = new URL(source.TranscodingUrl, base).searchParams;
  if (hevcPlays) {
    if (query.get("VideoCodec") !== "copy" || query.get("TranscodeReasons") !== "AudioCodecNotSupported") throw new Error(`not a copy: ${source.TranscodingUrl}`);
    if (!ffmpegRuns().every((r) => r.copy)) throw new Error("ffmpeg is not copying the video");
  } else if (query.get("VideoCodec") !== "h264" || query.get("TranscodeReasons") !== "VideoCodecNotSupported,AudioCodecNotSupported") {
    throw new Error(`not a re-encode for the video and audio codecs: ${source.TranscodingUrl}`);
  }
  await advances(page, 2);
  noErrors(seen);
  await leave(page);
});

// An HDR film is re-encoded to H.264 with its picture tone mapped to SDR BT.709
// (compat/captures/hdr-tonemap-12.4.md), whatever the client plays: HEVC in PQ and
// HLG, and 10-bit H.264 in PQ, which would otherwise be copied washed out. The
// tone-mapped picture is checked in the integration tier (tonemap_integration_test.go);
// here jellyfin-web plays it, the reasons are Jellyfin's, and ffmpeg is run with the filter.
// The expectation comes from the client's profile, which the runner states with
// E2E_HEVC: a profile that plays HEVC HDR direct plays the MP4 film (its container
// and audio fit), and every other case is the tone-mapped transcode. Resh's
// Matroska and AC3 never direct play, and an HDR film is never copied, so it is a
// transcode either way; its reasons are only checked for the profile without HEVC.
for (const [title, name, reasons, directWithHevc] of [
  ["Resh Film", "HDR10 HEVC in Matroska", "VideoCodecNotSupported,AudioCodecNotSupported", false],
  ["Shin Film", "HLG HEVC in MP4", "VideoCodecNotSupported", true],
  ["Tav Film", "HDR10 H.264 in Matroska", "VideoRangeTypeNotSupported", false],
]) {
  const direct = hevcPlays && directWithHevc;
  await scenario(`${name} ${direct ? "direct plays (E2E_HEVC=1)" : "plays through a tone-mapped transcode"}`, async ({ page, seen }) => {
    await play(page, "Transcode", title);
    const source = seen.infos.at(-1).source;
    if (direct) {
      if (source.SupportsDirectPlay !== true || source.TranscodingUrl) throw new Error(`not direct play: ${JSON.stringify(source)}`);
      await advances(page, 2);
      noErrors(seen);
      await leave(page);
      return;
    }
    const query = new URL(source.TranscodingUrl, base).searchParams;
    if (query.get("VideoCodec") !== "h264") throw new Error(`not a re-encode: ${source.TranscodingUrl}`);
    if ((!hevcPlays || title === "Tav Film") && query.get("TranscodeReasons") !== reasons) throw new Error(`reasons ${query.get("TranscodeReasons")}, want ${reasons}`);
    const runs = ffmpegRuns();
    if (runs.length !== 1 || runs[0].copy || !/tonemap=tonemap=hable/.test(runs[0].line) || !/-color_primaries bt709/.test(runs[0].line)) {
      throw new Error(`ffmpeg is not tone mapping: ${runs.map((r) => r.line.slice(0, 400))}`);
    }
    await advances(page, 4);
    const shown = await page.evaluate(() => {
      const v = document.querySelector("video");
      return { frames: v.getVideoPlaybackQuality().totalVideoFrames, width: v.videoWidth };
    });
    if (shown.frames < 20) throw new Error(`the picture did not decode: ${JSON.stringify(shown)}`);
    noErrors(seen);
    await leave(page);
  });
}

await scenario("a Matroska with no Cues has no index and is re-encoded", async ({ page, seen }) => {
  await play(page, "Transcode", "Digamma Film");
  const url = seen.infos.at(-1).source.TranscodingUrl;
  if (!/VideoCodec=h264/.test(url)) throw new Error(`a file with no index was offered a copy: ${url}`);
  const runs = ffmpegRuns();
  if (runs.length !== 1 || runs[0].copy || !/libx264/.test(runs[0].line)) throw new Error(`ffmpeg: ${runs.map((r) => r.line.slice(0, 300))}`);
  await advances(page, 2);
  noErrors(seen);
  await leave(page);
});

await scenario("a bitrate cap re-encodes a film that would be copied", async ({ page, seen }) => {
  await play(page, "Transcode", "Rho Film");
  if (!/VideoCodec=copy/.test(seen.infos.at(-1).source.TranscodingUrl)) throw new Error("Rho should be copied at Auto");
  const oldSource = await page.evaluate(() => document.querySelector("video")?.currentSrc);
  await page.mouse.move(600, 400);
  await page.click(".btnVideoOsdSettings");
  await page.click('[data-id="quality"]');
  await page.click('[data-id="420000"]');
  const info = await waitFor("a PlaybackInfo with the cap", async () => seen.infos.find((i) => i.request.MaxStreamingBitrate === 420000));
  if (!/VideoCodec=h264/.test(info.source.TranscodingUrl)) throw new Error(`a cap below the file was offered a copy: ${info.source.TranscodingUrl}`);
  await waitFor("the re-encode to run", async () => ffmpegRuns().some((r) => !r.copy && /libx264/.test(r.line)), 30000);
  await waitFor("only the re-encode to be left", async () => ffmpegRuns().length === 1, 20000);
  // The old stream, buffered, goes on playing while the new one loads, and the
  // new one then jumps from 0 to the resume point (Rho was played before), which
  // advances() would take for seconds of playing. jellyfin-web sends no Stopped
  // while it is changing stream (its isChangingStream, until the new stream's
  // play() resolves), so leaving then would wait for a Stopped that isn't sent.
  // A full gate timed out in leave() here on one machine; that has not recurred
  // since, with or without these waits, so they aren't shown to be what cured it.
  // Wait for the switch and the jump, then for playing.
  await waitFor("the player to switch to the capped stream", async () => (await page.evaluate(() => document.querySelector("video")?.currentSrc)) !== oldSource);
  await waitFor("the capped stream to start at the resume point", async () => (await time(page)) > 1, 30000);
  await advances(page, 2);
  noErrors(seen);
  await leave(page);
});

// What a copy saves. The worker's own ffmpeg time is its children's, read from
// the process table once they are gone.
function workerCpu() {
  const fields = readFileSync(`/proc/${process.env.E2E_WORKER_PID}/stat`, "utf8").replace(/^.*\) /, "").split(" ");
  return (Number(fields[13]) + Number(fields[14])) / 100; // cutime and cstime, in clock ticks.
}

// A zombie child still counts: pgrep -P matches on the parent alone.
function workerChildren() {
  try {
    return execFileSync("pgrep", ["-P", process.env.E2E_WORKER_PID]).toString().trim().split("\n").filter(Boolean).length;
  } catch {
    return 0; // pgrep exits 1 when nothing matches.
  }
}

async function cpuPerSegment(page, seen, title, segments, quality) {
  await play(page, "Transcode", title);
  if (quality) {
    await page.mouse.move(600, 400);
    await page.click(".btnVideoOsdSettings");
    await page.click('[data-id="quality"]');
    await page.click(`[data-id="${quality}"]`);
    await waitFor("the new stream", async () => seen.infos.length >= 2, 20000);
  }
  // The segments this stream's own jobs made. Any running job would count the
  // previous stream's, and a job's next_segment is a position in the film, so a
  // job that starts past 0 would count segments it never made: either way the
  // wait ends at once, measuring 0.11 CPU s of re-encode where a real one takes 18.
  const stored = () => Number(sql(`SELECT COALESCE(sum(next_segment - start_segment), 0) FROM transcode_jobs WHERE owner = '${seen.infos.at(-1).session}'`));
  await waitFor(`${segments} segments`, async () => stored() >= segments, 120000);
  await leave(page);
  await waitFor("ffmpeg to exit", async () => ffmpegRuns().length === 0, 30000);
  // pgrep stops seeing a killed ffmpeg as soon as it is a zombie (its command
  // line is gone), but the kernel adds its CPU time to the worker's cutime only
  // when the worker reaps it. Reading before then measured a 22 s re-encode as 0.11.
  await waitFor("the worker to reap its ffmpeg", async () => workerChildren() === 0, 30000);
}

await scenario("a copied 1080p film costs a fraction of the CPU of the re-encode", async ({ page, seen }) => {
  const segments = 8; // 32 s of film each way.
  let before = workerCpu();
  await cpuPerSegment(page, seen, "Stigma Film", segments, null);
  const copy = workerCpu() - before;
  const copyRuns = seen.infos.at(-1).source.TranscodingUrl;
  if (!/VideoCodec=copy/.test(copyRuns)) throw new Error("the first run was not a copy");
  const second = await viewer();
  try {
    before = workerCpu();
    await cpuPerSegment(second.page, second.seen, "Stigma Film", segments, 3000000);
    const encoded = workerCpu() - before;
    if (!/VideoCodec=h264/.test(second.seen.infos.at(-1).source.TranscodingUrl)) throw new Error("the second run was not a re-encode");
    console.log(`      1080p, ${segments * 4} s of film: copy ${copy.toFixed(2)} CPU s, re-encode ${encoded.toFixed(2)} CPU s (${(copy / encoded * 100).toFixed(0)}%)`);
    if (copy > encoded * 0.25) throw new Error(`a copy used ${copy.toFixed(2)} CPU s, a re-encode ${encoded.toFixed(2)}`);
  } finally {
    await second.context.close().catch(() => {});
  }
});

// How long a seek takes on a copied 1080p film, from POSIX and from S3: resuming
// in the middle starts a job there, as a seek past what is made does. A copy
// reads its input at 5 times the film's speed on its own, so the shim's slower
// rate does not apply to it.
for (const [where, library, title] of [["POSIX", "Transcode", "Sho Film"], ["S3", "Cloud", "Heh Film"]]) {
  await scenario("a copied 1080p film resumes in the middle" + `: ${where}`, async ({ page, seen }) => {
    const { firstFrameMs, starts } = await resume(page, library, title, 40);
    if (!/VideoCodec=copy/.test(seen.infos.at(-1).source.TranscodingUrl)) throw new Error("not a copy");
    console.log(`      ${where}, 1080p copied: time to first frame on resume at 40 s of 60 s: ${firstFrameMs} ms; ffmpeg started at ${starts}`);
    noErrors(seen);
    await leave(page);
  });
}

await scenario("the quality menu cap transcodes a film that direct plays", async ({ page, seen }) => {
  await play(page, "Movies", "Alpha Film");
  if (seen.infos.at(-1).source.TranscodingUrl !== undefined) throw new Error("Alpha should direct play at Auto");
  await page.mouse.move(600, 400);
  await page.click(".btnVideoOsdSettings");
  await page.click('[data-id="quality"]');
  await page.click('[data-id="420000"]');
  const info = await waitFor("a PlaybackInfo with the cap", async () => seen.infos.find((i) => i.request.MaxStreamingBitrate === 420000));
  const query = new URL(info.source.TranscodingUrl, base).searchParams;
  if (!/ContainerBitrateExceedsLimit/.test(query.get("TranscodeReasons") ?? "")) throw new Error(`reasons: ${query.get("TranscodeReasons")}`);
  if (Number(query.get("VideoBitrate")) + Number(query.get("AudioBitrate")) !== 420000) throw new Error(`split: ${query.get("VideoBitrate")} + ${query.get("AudioBitrate")}`);
  await waitFor("the transcode to play", async () => seen.hls.some((r) => /\/hls1\/main\/\d+\.mp4$/i.test(r.path) && r.status === 200));
  await advances(page);
  noErrors(seen);
  await leave(page);
});

for (const flavour of flavours) await scenario("switching audio plays the new track and the old job stops" + `: ${flavour.name}`, async ({ page, seen }) => {
  await play(page, "Transcode", flavour.audio);
  await advances(page);
  const before = ffmpegRuns();
  if (before.length !== 1 || before[0].audio !== "1") throw new Error(`expected one ffmpeg on track 1: ${JSON.stringify(before)}`);
  const oldSession = seen.infos.at(-1).session;
  await page.mouse.move(600, 400);
  await page.click(".btnAudio");
  await page.click('[data-id="2"]');
  const info = await waitFor("a PlaybackInfo for track 2", async () => seen.infos.find((i) => Number(i.request.AudioStreamIndex) === 2));
  if (info.session === oldSession) throw new Error("the session id did not change");
  await waitFor("DELETE ActiveEncodings for the old session", async () => seen.deletes.some((u) => u.includes(oldSession)));
  await waitFor("segments of the new stream", async () => seen.hls.some((r) => r.query.get("AudioStreamIndex") === "2" && /\/hls1\/main\/\d+\.mp4$/i.test(r.path) && r.status === 200));
  await advances(page);
  await waitFor("only the new track's ffmpeg to be left", async () => {
    const runs = ffmpegRuns();
    return runs.length === 1 && runs[0].audio === "2";
  }, 20000).catch(() => { throw new Error(`ffmpeg runs after the switch: ${JSON.stringify(ffmpegRuns())}`); });
  noErrors(seen);
  await leave(page);
});

// ---- Copied audio under a re-encoded picture ----
// The packets of an audio stream: time, then size and hash.
function audioPacketsOf(file, stream) {
  return execFileSync("ffprobe", ["-v", "error", "-select_streams", stream, "-show_entries", "packet=pts_time,size,data_hash", "-show_data_hash", "sha256", "-of", "csv=p=0", file]).toString()
    .trim().split("\n").map((line) => { const f = line.split(","); return { time: parseFloat(f[0]), data: `${f[1]} ${f[2]}` }; });
}

// The film is interlaced, so its video is re-encoded; Chrome plays its AAC, so the
// audio is copied (compat/captures/audio-copy-12.4.md). Through a far seek, which
// starts a job in the middle, and a switch to the second track.
await scenario("audio copy: an interlaced film re-encodes the picture and copies the AAC audio through a seek and a track switch", async ({ page, seen }) => {
  const file = `${media}/Audio Copy Film (2090)/Audio Copy Film (2090).mkv`;
  await play(page, "Transcode", "Audio Copy Film");
  const source = seen.infos.at(-1).source;
  const url = source.TranscodingUrl;
  if (!/VideoCodec=h264/.test(url) || !/AudioCodec=aac,opus,flac&/.test(url) || !/TranscodingMaxAudioChannels=2/.test(url) || !/TranscodeReasons=InterlacedVideoNotSupported$/.test(url)) {
    throw new Error(`not a re-encode with AAC to copy, and no audio reason: ${url}`);
  }
  const master = await (await fetch(new URL(url, base))).text();
  if (!/CODECS="avc1\.[0-9A-F]+,mp4a\.40\.2"/.test(master)) throw new Error(`master: ${master}`);
  const copying = (run, track) => /libx264/.test(run.line) && / -c:a copy /.test(run.line) && !/-c:a aac/.test(run.line) && run.audio === track;
  const first = ffmpegRuns();
  if (first.length !== 1 || !copying(first[0], "1")) throw new Error(`ffmpeg is not copying the audio: ${first.map((r) => r.line.slice(0, 500))}`);
  await advances(page, 2);
  noErrors(seen);

  const total = await duration(page);
  const target = total * 0.6;
  await page.evaluate((t) => { document.querySelector("video").currentTime = t; }, target);
  await waitFor("playback past the seek target", async () => (await time(page)) > target + 1.5, 60000);
  const restarted = await waitFor("the job that started at the seek", async () => {
    const runs = ffmpegRuns();
    return runs.length === 1 && runs[0].start >= Math.floor(target / 4) - 2 ? runs[0] : null;
  }, 20000);
  // A job that starts in the middle copies the audio too, dropping what falls before its first segment.
  if (!copying(restarted, "1") || !/-bsf:a noise=drop=lt\(pts\*tb/.test(restarted.line)) throw new Error(`the restart: ${restarted.line.slice(0, 600)}`);
  noErrors(seen);

  // The pointer has been still since the last menu, so it has to move to show the controls.
  await page.mouse.move(300, 300);
  await page.mouse.move(600, 400);
  await page.click(".btnAudio");
  await page.click('[data-id="2"]');
  await waitFor("segments of the second track", async () => seen.hls.some((r) => r.query.get("AudioStreamIndex") === "2" && /\/hls1\/main\/\d+\.mp4$/i.test(r.path) && r.status === 200));
  await advances(page);
  const switched = await waitFor("only the second track's ffmpeg to be left", async () => {
    const runs = ffmpegRuns();
    return runs.length === 1 && runs[0].audio === "2" ? runs[0] : null;
  }, 20000);
  if (!copying(switched, "2")) throw new Error(`the second track is not copied: ${switched.line.slice(0, 500)}`);

  // What the player was given is the file's own packets of that track, in order.
  const served = seen.hls.filter((r) => r.query.get("AudioStreamIndex") === "2" && /\/hls1\/main\/\d+\.mp4$/i.test(r.path) && r.status === 200).at(-1);
  const playlist = await playlistOf(transcodeUrl(seen));
  const index = Number(/\/(\d+)\.mp4$/.exec(served.path)[1]);
  const init = await bytesOf(playlist.init);
  const body = await bytesOf(playlist.segments[index].url);
  const dir = mkdtempSync(`${tmpdir()}/jellyscale-e2e-`);
  writeFileSync(`${dir}/segment.mp4`, Buffer.concat([init, body]));
  const got = audioPacketsOf(`${dir}/segment.mp4`, "a:0");
  rmSync(dir, { recursive: true, force: true });
  const original = audioPacketsOf(file, "a:1");
  // Found by time, since a tone repeats its packets; the output's clock is the file's plus 100 s.
  const at = original.findIndex((packet) => Math.abs(packet.time - (got[0].time - 100)) < 0.01);
  if (at < 0 || got.some((packet, i) => packet.data !== original[at + i]?.data || Math.abs(packet.time - 100 - original[at + i].time) > 0.002)) throw new Error(`segment ${index} is not a run of the file's second track (${got.length} packets, first at ${at})`);
  noErrors(seen);
  await leave(page);
});

// Opus and FLAC are copied for Chrome, whose profile lists them: playing and seeking
// show that its decoder takes the track as it is in fMP4.
for (const [codec, title] of [["opus", "Audio Opus Film"], ["flac", "Audio Flac Film"]]) {
  await scenario(`audio copy: an interlaced film re-encodes the picture and copies ${codec} audio, playing and seeking`, async ({ page, seen }) => {
    await play(page, "Transcode", title);
    const url = seen.infos.at(-1).source.TranscodingUrl;
    if (!/VideoCodec=h264/.test(url) || !/AudioCodec=aac,opus,flac&/.test(url) || !/TranscodeReasons=InterlacedVideoNotSupported$/.test(url)) throw new Error(`not a re-encode: ${url}`);
    const master = await (await fetch(new URL(url, base))).text();
    const named = codec === "opus" ? "Opus" : "fLaC";
    if (!master.includes(`,${named}"`)) throw new Error(`master: ${master}`);
    const copying = (run) => /libx264/.test(run.line) && / -c:a copy /.test(run.line) && !/-c:a aac/.test(run.line);
    const first = ffmpegRuns();
    if (first.length !== 1 || !copying(first[0])) throw new Error(`ffmpeg is not copying the audio: ${first.map((r) => r.line.slice(0, 500))}`);
    await advances(page, 2);
    noErrors(seen);
    const target = (await duration(page)) * 0.6;
    await page.evaluate((t) => { document.querySelector("video").currentTime = t; }, target);
    await waitFor("playback past the seek target", async () => (await time(page)) > target + 1.5, 60000);
    const restarted = await waitFor("the job that started at the seek", async () => {
      const runs = ffmpegRuns();
      return runs.length === 1 && runs[0].start >= Math.floor(target / 4) - 2 ? runs[0] : null;
    }, 20000);
    if (!copying(restarted)) throw new Error(`the restart: ${restarted.line.slice(0, 600)}`);
    await advances(page, 2);
    noErrors(seen);
    await leave(page);
  });
}

// The copied track counts against the cap by its own bitrate (AAC at 128 kbps in
// the MP4): a 720 kbps cap fits it, a 420 kbps one doesn't.
await scenario("audio copy: a bitrate cap the track doesn't fit re-encodes the audio, and one it fits copies it", async ({ page, seen }) => {
  await play(page, "Transcode", "Audio Cap Film");
  const copying = (line) => / -c:a copy /.test(line);
  const encoding = (line) => /-c:a aac/.test(line);
  const only = (test) => async () => { const runs = ffmpegRuns(); return runs.length === 1 && test(runs[0].line); };
  await waitFor("audio copied at Auto", only(copying), 20000);
  const auto = transcodeUrl(seen);
  if (!/AudioBitrate=1[0-9]{5}&/.test(auto) || !/VideoCodec=h264/.test(auto)) throw new Error(`Auto: ${auto}`);
  const choose = async (id) => {
    await page.mouse.move(600, 400);
    await page.click(".btnVideoOsdSettings");
    await page.click('[data-id="quality"]');
    await page.click(`[data-id="${id}"]`);
    const info = await waitFor(`a PlaybackInfo with the cap ${id}`, async () => seen.infos.find((i) => i.request.MaxStreamingBitrate === Number(id)));
    return info.source.TranscodingUrl;
  };
  const low = await choose("420000");
  if (!/AudioBitrate=84000&/.test(low)) throw new Error(`420 kbps: ${low}`);
  await waitFor("the audio re-encoded under a 420 kbps cap", only(encoding), 30000);
  await advances(page, 2);
  const fits = await choose("720000");
  if (!/AudioBitrate=1[0-9]{5}&/.test(fits)) throw new Error(`720 kbps: ${fits}`);
  await waitFor("the audio copied under a 720 kbps cap", only(copying), 30000);
  await advances(page, 2);
  noErrors(seen);
  await leave(page);
});

await scenario("closing the tab ends ffmpeg once the idle time passes", async ({ context, page, seen }) => {
  await play(page, "Transcode", "Iota Film");
  await advances(page);
  if (ffmpegRuns().length !== 1) throw new Error("no ffmpeg to end");
  await page.close(); // Sends no Stopped and no DELETE.
  await new Promise((resolve) => setTimeout(resolve, 2000));
  if (ffmpegRuns().length !== 1) throw new Error("ffmpeg ended before the idle time, so this proves nothing");
  await waitFor("ffmpeg to end", async () => ffmpegRuns().length === 0, 45000);
});

await scenario("DELETE ActiveEncodings ends ffmpeg", async ({ page, seen }) => {
  await play(page, "Transcode", "Kappa Film");
  await advances(page);
  if (ffmpegRuns().length !== 1) throw new Error("no ffmpeg to end");
  const session = seen.infos.at(-1).session;
  const status = await page.evaluate(async (id) => {
    const client = window.ApiClient;
    // Twice, as jellyfin-web sends it.
    const calls = [0, 1].map(() => fetch(`${client.serverAddress()}/Videos/ActiveEncodings?deviceId=${encodeURIComponent(client.deviceId())}&PlaySessionId=${id}`,
      { method: "DELETE", headers: { Authorization: `MediaBrowser Token="${client.accessToken()}"` } }).then((r) => r.status));
    return Promise.all(calls);
  }, session);
  if (status.join() !== "204,204") throw new Error(`DELETE answered ${status}`);
  await waitFor("ffmpeg to end", async () => ffmpegRuns().length === 0, 20000);
});

// Two viewers on one spec share a job, and stopping one leaves the other.
const twoViewers = "two viewers share a job and stopping one leaves the other playing";
if (!process.env.E2E_ONLY) {
  const a = await viewer();
  const b = await viewer();
  try {
    await play(a.page, "Transcode", "Lambda Film");
    await play(b.page, "Transcode", "Lambda Film");
    await advances(a.page, 1);
    await advances(b.page, 1);
    if (ffmpegRuns().length < 1) throw new Error("no ffmpeg is running for two viewers");
    await leave(a.page);
    // Several heartbeats pass.
    await new Promise((resolve) => setTimeout(resolve, 6000));
    await advances(b.page, 2);
    if (ffmpegRuns().length < 1) throw new Error("stopping one viewer ended the other's ffmpeg");
    noErrors(b.seen);
    await leave(b.page);
    await waitFor("ffmpeg to end once both left", async () => ffmpegRuns().length === 0, 20000);
    console.log(`ok    ${twoViewers}`);
  } catch (error) {
    failures.push(`${twoViewers}: ${error.message.split("\n")[0]}`);
    console.log(`FAIL  ${twoViewers}: ${error.message.split("\n")[0]}`);
  } finally {
    await a.context.close().catch(() => {});
    await b.context.close().catch(() => {});
  }
}

// Reports a position as a player leaving would, then opens the film from its
// details page and presses Resume. Returns how long the first frame took.
async function resume(page, library, title, seconds) {
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: library }).first().click();
  await page.locator(".cardText a:visible", { hasText: title }).first().click();
  await page.locator(".btnPlay:visible").first().waitFor();
  const id = new URLSearchParams(page.url().split("?")[1]).get("id");
  await page.evaluate(async ([id, ticks]) => {
    const client = window.ApiClient;
    await fetch(`${client.serverAddress()}/Sessions/Playing/Stopped`, {
      method: "POST",
      headers: { Authorization: `MediaBrowser Token="${client.accessToken()}"`, "Content-Type": "application/json" },
      body: JSON.stringify({ ItemId: id, PositionTicks: ticks }),
    });
  }, [id, seconds * 10_000_000]);
  await page.reload();
  const button = page.locator(".btnPlay[data-action=resume]:visible").first();
  await button.waitFor({ timeout: 30000 });
  const began = Date.now();
  const started = page.waitForResponse((r) => r.request().method() === "POST" && /\/Sessions\/Playing$/i.test(new URL(r.url()).pathname), { timeout: 60000 });
  await button.click();
  // Every ffmpeg start number seen while it loads.
  const starts = new Set();
  await waitFor("the film to play from the resume point", async () => {
    for (const run of ffmpegRuns()) starts.add(run.start);
    return (await time(page)) > seconds + 0.1;
  }, 60000);
  // The player reports its start just after the video starts; leaving before
  // that goes back before it has anything to stop.
  await started;
  return { firstFrameMs: Date.now() - began, starts: [...starts] };
}

for (const flavour of flavours) await scenario("resuming mid-film starts one job, at the resume point" + `: ${flavour.name}`, async ({ page, seen }) => {
  const { firstFrameMs, starts } = await resume(page, "Transcode", flavour.resume, 40);
  console.log(`      time to first frame on resume: ${firstFrameMs} ms; ffmpeg start segments seen: ${starts}`);
  if (starts.length !== 1 || starts[0] !== 10) throw new Error(`ffmpeg started at ${starts}, want only segment 10`);
  noErrors(seen);
  await leave(page);
});

// A transcode that breaks must end in the dialog, not in endless asking.
await scenario("a transcode that fails: the viewer gets the dialog, and the client stops asking", async ({ page, seen }) => {
  const media = process.env.E2E_MEDIA;
  if (!media) throw new Error("E2E_MEDIA is the transcode library directory");
  const file = `${media}/Xi Film (2031)/Xi Film (2031).mkv`;
  // The scanner's probe is in the database; the file the worker reads is not media.
  writeFileSync(file, readFileSync(file).subarray(0, 4096));
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: "Transcode" }).first().click();
  await page.locator(".cardText a:visible", { hasText: "Xi Film" }).first().click();
  await page.locator(".btnPlay:visible").first().click();
  await page.getByText("Playback Error").first().waitFor({ timeout: 60000 });
  await new Promise((resolve) => setTimeout(resolve, 8000));
  if (seen.infos.length > 4) throw new Error(`the client asked ${seen.infos.length} times`);
  if (seen.hls.some((r) => r.status === 200 && /\/hls1\/main\/\d+\.mp4$/i.test(r.path))) throw new Error("a broken file produced a segment");
});

// A worker whose transcodeStore differs from the replicas': refused, with the
// cause in the log. The row is a worker that stays alive.
await scenario("a worker with another transcodeStore: the dialog, and the log names the cause", async ({ page, seen }) => {
  if (!process.env.E2E_PG) throw new Error("E2E_PG is the Postgres container");
  sql("INSERT INTO transcode_nodes (id, roles, slots, store, seen_at) VALUES ('elsewhere', 'worker', 2, 'file:///other/segments', now() + interval '1 hour')");
  try {
    await page.goto(`${base}/web/`);
    await page.waitForSelector(".card", { timeout: 30000 });
    await page.locator(".card:visible", { hasText: "Transcode" }).first().click();
    await page.locator(".cardText a:visible", { hasText: "Theta Film" }).first().click();
    await page.locator(".btnPlay:visible").first().click();
    await page.getByText("Playback Error").first().waitFor({ timeout: 20000 });
    await new Promise((resolve) => setTimeout(resolve, 5000));
    if (seen.infos.length > 2) throw new Error(`the client asked ${seen.infos.length} times`);
    if (seen.hls.length) throw new Error("the client got a stream it could not play");
    const log = readFileSync(process.env.E2E_LOG, "utf8");
    if (!log.includes("file:///other/segments") || !log.includes("must be the same on every role")) throw new Error("the log doesn't name the worker's store");
  } finally {
    sql("DELETE FROM transcode_nodes WHERE id = 'elsewhere'");
  }
});

// ffmpeg that hangs, with the worker still up and renewing the lease. The
// browser abandons a request after about ten seconds and sends it again, so
// the server's own deadline never reaches it; the worker has to fail the job.
await scenario("ffmpeg hangs: the viewer gets the dialog, and the client stops asking", async ({ page, seen }) => {
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: "Transcode" }).first().click();
  await page.locator(".cardText a:visible", { hasText: "Omicron Film" }).first().click();
  await page.locator(".btnPlay:visible").first().click();
  await waitFor("ffmpeg to start", async () => ffmpegRuns().length === 1, 20000);
  for (const pid of execFileSync("pgrep", ["-f", `ffmpeg.*${scratch}`]).toString().trim().split("\n")) process.kill(Number(pid), "SIGSTOP");
  await page.getByText("Playback Error").first().waitFor({ timeout: 90000 });
  await new Promise((resolve) => setTimeout(resolve, 8000));
  if (seen.infos.length > 3) throw new Error(`the client asked ${seen.infos.length} times`);
  if (!readFileSync(process.env.E2E_LOG, "utf8").includes("ffmpeg made no segment")) throw new Error("the log doesn't say ffmpeg stalled");
});

// A stream replica with no transcodeStore registers that, and the api replicas
// find the difference among the roles, so the viewer gets the dialog and not a
// loop.
await scenario("a stream replica without transcodeStore: the dialog, and the log names the cause", async ({ page, seen }) => {
  const nostore = process.env.E2E_BASE_NOSTORE;
  if (!nostore || !process.env.E2E_NOSTORE) throw new Error("E2E_NOSTORE starts a stream replica without a transcodeStore, behind E2E_BASE_NOSTORE");
  const replica = spawn(process.env.E2E_NOSTORE, [], { stdio: "ignore" });
  base = nostore;
  try {
    await waitFor("the replica to register", async () => sql("SELECT count(*) FROM transcode_nodes WHERE roles = 'stream' AND store = ''") === "1", 30000);
    // A new login, as the viewer was made against the other router.
    await login(page);
    await page.locator(".card:visible", { hasText: "Transcode" }).first().click();
    await page.locator(".cardText a:visible", { hasText: "Theta Film" }).first().click();
    await page.locator(".btnPlay:visible").first().click();
    await page.getByText("Playback Error").first().waitFor({ timeout: 30000 });
    await new Promise((resolve) => setTimeout(resolve, 8000));
    if (seen.infos.length > 3) throw new Error(`the client asked ${seen.infos.length} times`);
    if (!readFileSync(process.env.E2E_LOG, "utf8").includes("stream has no transcodeStore")) throw new Error("the log doesn't name the missing store");
  } finally {
    base = home;
    const gone = new Promise((resolve) => replica.once("exit", resolve));
    replica.kill("SIGTERM");
    await gone;
  }
});

// Last, because it stops the worker. A viewer whose transcode can't start must
// be told, not left to jellyfin-web's retries, which never end on their own.
if (!process.env.E2E_ONLY || /no worker/i.test(process.env.E2E_ONLY)) {
  // The worker goes away while a film plays. The player abandons a request
  // after ten seconds, so a server that answers later leaves it asking for
  // ever; it must answer sooner. jellyfin-web shows no dialog for an error
  // after playback began: it drops the stream, so the picture stops and the
  // client asks for nothing more. The log has to say why.
  await scenario("worker lost mid-film: the player gets an answer in time and stops asking", async ({ page, seen }) => {
    await play(page, "Transcode", "Pi Film");
    await advances(page);
    process.kill(workerPid, "SIGKILL");
    const target = (await duration(page)) * 0.8;
    const seeked = Date.now();
    await page.evaluate((t) => { document.querySelector("video").currentTime = t; }, target);
    const answer = await waitFor("an error answer to the segment", async () => seen.hls.find((r) => r.status >= 500 && /\/hls1\/main\/\d+\.mp4$/i.test(r.path)), 30000);
    console.log(`      worker killed, far seek: the segment request was answered ${answer.status} after ${answer.at - seeked} ms`);
    if (answer.at - seeked > 9000) throw new Error(`the answer took ${answer.at - seeked} ms, and the player gives up at 10000`);
    const asked = [seen.hls.length, seen.infos.length];
    await new Promise((resolve) => setTimeout(resolve, 12000));
    if (seen.hls.length !== asked[0] || seen.infos.length !== asked[1]) throw new Error("the client kept asking after the refusal");
    // The dead worker still counts as a worker for 10 seconds, so the log may say its slots were busy.
    if (!/a transcode job found (no worker|every slot busy)/.test(readFileSync(process.env.E2E_LOG, "utf8"))) throw new Error("the log doesn't say why the job found no worker");
  });

  await scenario("no worker: the viewer gets a playback error dialog, and the client stops asking", async ({ page, seen }) => {
    try { process.kill(workerPid, "SIGTERM"); } catch {} // Already gone after the scenario above.
    await new Promise((resolve) => setTimeout(resolve, 11000)); // The server forgets a worker after 10 s.
    await page.goto(`${base}/web/`);
    await page.waitForSelector(".card", { timeout: 30000 });
    await page.locator(".card:visible", { hasText: "Transcode" }).first().click();
    await page.locator(".cardText a:visible", { hasText: "Theta Film" }).first().click();
    await page.locator(".btnPlay:visible").first().click();
    await page.getByText("Playback Error").first().waitFor({ timeout: 20000 });
    await new Promise((resolve) => setTimeout(resolve, 5000));
    if (seen.infos.length > 2) throw new Error(`the client asked ${seen.infos.length} times`);
    if (seen.hls.length) throw new Error("the client got a stream it could not play");
  });
}

// Still without a worker: a film that direct plays keeps playing when the
// viewer picks its other audio track, which would have needed a transcode.
if (!process.env.E2E_ONLY || /no worker/i.test(process.env.E2E_ONLY)) {
  await scenario("no worker: switching audio on a direct play film keeps it playing", async ({ page, seen }) => {
    await play(page, "Transcode", "Nu Film");
    if (seen.infos.at(-1).source.TranscodingUrl !== undefined) throw new Error("Nu Film should direct play");
    await advances(page);
    await page.mouse.move(600, 400);
    await page.click(".btnAudio");
    await page.click('[data-id="2"]');
    await waitFor("a PlaybackInfo for track 2", async () => seen.infos.find((i) => Number(i.request.AudioStreamIndex) === 2));
    if (await page.getByText("Playback Error").count()) throw new Error("the viewer got an error dialog");
    await new Promise((resolve) => setTimeout(resolve, 3000));
    await advances(page, 2);
    if (seen.infos.length > 3) throw new Error(`the client asked ${seen.infos.length} times`);
    await leave(page);
  });
}

await browser.close();
if (failures.length) {
  console.log("\nFAILED");
  for (const failure of failures) console.log(`  - ${failure}`);
  process.exit(1);
}
console.log("\nPASS");
