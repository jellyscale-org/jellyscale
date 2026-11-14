// Phase 3 gate: unmodified jellyfin-web plays films Chrome can't direct-play,
// through the router with two api replicas, a stream replica and a worker on
// its own process, and the transcode ends when it should.
// Usage: E2E_SCRATCH=<worker scratch dir> node transcode.mjs <base_url> <user> <password>
// Run from a directory with playwright-core installed.
import { execFile, execFileSync, spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { createServer } from "node:net";
import { dirname } from "node:path";
import { promisify } from "node:util";
import { createRequire } from "node:module";
import { advances, time, waitFor } from "./player.mjs";
import { runLater, schedule, shardLine } from "./shard.mjs";
import { recordResponses } from "./conform.mjs";

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
      // The audio track's map: from the second input, or from the only one.
      audio: /-map [01]:(\d+)/.exec(line)?.[1],
      inputs: (line.match(/ -i /g) ?? []).length,
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

function clock() { return Date.now(); }

// A signed-in browser that records what the server told it.
async function viewer() {
  const context = await browser.newContext({ viewport: { width: 1280, height: 720 } });
  const page = await context.newPage();
  recordResponses(page, base);
  const seen = { infos: [], hls: [], deletes: [], errors: [], console: [], failed: [], requests: [], answers: [], cut: [], open: new Set() };
  page.on("pageerror", (error) => seen.errors.push(error.message));
  // A request that ended without an answer: hls.js abandons one after ten seconds. A seek aborts some too, so a scenario that seeks looks before it does.
  page.on("requestfailed", (request) => { if (request.url().startsWith(base) && /\/hls1\//i.test(request.url())) seen.failed.push(`${request.method()} ${new URL(request.url()).pathname} ${request.failure()?.errorText}`); });
  page.on("console", (message) => { if (/hls|playback/i.test(message.text())) seen.console.push(message.text().slice(0, 200)); });
  // The player's playback requests, for a failure to show what it did and when.
  const start = Date.now();
  page.timeline = [];
  // Every request to the server and every answer, whichever role gave it, for the scenarios that put Postgres away.
  page.on("request", (request) => { if (request.url().startsWith(base)) { seen.requests.push({ at: clock(), method: request.method(), url: request.url(), range: request.headers().range }); seen.open.add(request); } });
  // A request that ended without an answer (a response cut, a connection refused), and every request still waiting for its end.
  page.on("requestfinished", (request) => seen.open.delete(request));
  page.on("requestfailed", (request) => { seen.open.delete(request); if (request.url().startsWith(base)) seen.cut.push({ at: clock(), method: request.method(), url: request.url(), error: request.failure()?.errorText }); });
  page.on("response", (response) => { if (response.url().startsWith(base)) seen.answers.push({ at: clock(), status: response.status(), method: response.request().method(), url: response.url(), routed: response.headers()["x-routed-to"], backend: response.headers()["x-backend"] }); });
  page.on("request", (request) => {
    if (request.method() === "DELETE" && /ActiveEncodings/i.test(request.url())) seen.deletes.push(request.url());
    const url = new URL(request.url());
    if (/PlaybackInfo|Sessions\/Playing|ActiveEncodings|master\.m3u8|main\.m3u8/i.test(url.pathname)) {
      page.timeline.push(`${Date.now() - start} ms ${request.method()} ${url.pathname.replace(/^.*\/(?=[^/]+\/?[^/]*$)/, "")} ${url.searchParams.get("PlaySessionId") ?? ""}`);
    }
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

const duration = (page) => page.evaluate(() => document.querySelector("video")?.duration ?? 0);

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
  const bad = seen.hls.filter((r) => r.status >= 400 && r.status !== 410);
  if (bad.length) throw new Error(`HLS requests failed: ${JSON.stringify(bad.slice(0, 3))}`);
}

const transcodeUrl = (seen) => seen.infos.map((i) => i.source.TranscodingUrl).filter(Boolean).at(-1);

// E2E_ONLY="audio" runs just the scenarios whose name matches. A name is a quoted
// literal, with any part known only at run time after ": ", so that test/report.sh
// can annotate the scenario at its own line.
// E2E_SHARD (see shard.mjs) picks the scenarios by call order, so a scenario that
// stops the worker or needs it gone is marked { stopsWorker: true } at its call
// and runs last, on the last shard.
function scenario(name, run, options = {}) {
  return schedule(name, options, async () => {
    const began = Date.now();
    const v = await viewer();
    let passed = false;
    try {
      await run(v);
      passed = true;
    } catch (error) {
      failures.push(`${name}: ${error.message.split("\n")[0]}`);
      console.log(`FAIL  ${name}: ${error.message.split("\n")[0]} (${((Date.now() - began) / 1000).toFixed(1)}s)`);
      await v.page.screenshot({ path: `${process.env.TMPDIR ?? "/tmp"}/jellyscale-e2e-transcode.png` }).catch(() => {});
    } finally {
      await v.context.close().catch(() => {});
    }
    // Nothing may be left running for the next scenario.
    await waitFor("ffmpeg to exit between scenarios", async () => ffmpegRuns().length === 0, 60000).catch(() => {
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
  page.timeline?.push("back");
  await page.evaluate(() => window.history.back());
  const response = await stopped.catch(async () => {
    // jellyfin-web sends nothing if it thinks the stream is changing, or if its
    // player page never saw the start. This says how far the page got.
    const video = await page.evaluate(() => {
      const v = document.querySelector("video");
      return v ? `a video at ${v.currentTime} s, paused ${v.paused}` : "no video";
    });
    throw new Error(`no Stopped within 15 s of going back: the page is ${new URL(page.url()).hash} with ${video}; the player's last requests: ${page.timeline.slice(-12).join("; ")}`);
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

// ---- What the admin dashboard and the Playback info panel say about a stream (JS-067) ----
// Both read TranscodingInfo from GET /Sessions: the panel as the player draws it,
// and the dashboard's session card in a dialog its info button opens. The texts
// are jellyfin-web's own (en-US), so a field that is missing or wrong shows here.
const reasonTexts = { AudioCodecNotSupported: "The audio codec is not supported", VideoCodecNotSupported: "The video codec is not supported", InterlacedVideoNotSupported: "Interlaced video is not supported" };

// The sections of the player's Playback info panel: a title and its label/value rows.
async function playbackPanel(page, until) {
  await page.mouse.move(600, 400);
  await page.click(".btnVideoOsdSettings");
  await page.click('.actionSheetMenuItem[data-id="stats"]', { timeout: 10000 });
  const read = () => page.evaluate(() => {
    const sections = [];
    for (const row of document.querySelectorAll(".playerStats-stat")) {
      const label = row.querySelector(".playerStats-stat-label")?.innerText.trim() ?? "";
      const value = row.querySelector(".playerStats-stat-value")?.innerText.trim() ?? "";
      if (row.classList.contains("playerStats-stat-header")) sections.push({ title: label, rows: {} });
      else if (sections.length) sections.at(-1).rows[label] = value;
    }
    return sections;
  });
  // The panel asks for the session again every few seconds, and the first
  // answer can come before the transcode has started.
  const sections = await waitFor("the Playback info panel to show the transcode", async () => {
    const found = await read();
    return until(found) ? found : null;
  }, 40000).catch(async (error) => { throw new Error(`${error.message}; the panel shows ${JSON.stringify(await read())}`); });
  return sections;
}

// What the dashboard's card for the film says when its info button is pressed.
async function dashboardDialog(title) {
  const admin = await viewer();
  try {
    await admin.page.goto(`${base}/web/#/dashboard`);
    const card = admin.page.locator(".MuiCard-root", { hasText: title }).first();
    await card.waitFor({ timeout: 40000 });
    await card.locator(".MuiCardActions-root button").last().click();
    const dialog = admin.page.locator("[role=dialog]").last();
    await dialog.waitFor({ timeout: 10000 });
    return (await dialog.innerText()).replace(/\s+/g, " ");
  } finally {
    await admin.context.close().catch(() => {});
  }
}

// What jellyfin-web 12.4 renders (web-dist chunk 1836, function m): for a
// re-encode, Target codecs adds the channels and bitrate and a Progress / speed
// row shows "n% / f fps (r.rrx)"; for a copy (DirectStream) neither, and its
// dashboard dialog is generic help text with no TranscodingInfo details.
const infoFlavours = [
  { name: "re-encoded", film: "Zeta Interlaced", method: "Transcoding", panel: "Transcoding Info", codecs: /^H264, AAC, \d+ Ch, \d+(\.\d Mbps| Kbps)$/, transcode: true },
  { name: "copied", film: "Zeta Film", method: "Direct streaming", panel: "Direct Streaming Info", codecs: /^H264 \(direct\), AAC$/, transcode: false },
];
for (const flavour of infoFlavours) await scenario("the dashboard card and the Playback info panel show the transcode" + `: ${flavour.name}`, async ({ page, seen }) => {
  // Sessions of earlier scenarios may still say they play a film; only this one should.
  sql("UPDATE sessions SET item_id = NULL, play_method = NULL, play_session_id = NULL");
  await play(page, "Transcode", flavour.film);
  await advances(page);
  const reasons = /TranscodeReasons=([^&]+)/.exec(transcodeUrl(seen))?.[1].split(",") ?? [];
  if (!reasons.length || reasons.some((r) => !reasonTexts[r])) throw new Error(`reasons the test has no text for: ${reasons}`);

  const sections = await playbackPanel(page, (found) => found.some((x) => x.title === flavour.panel && x.rows["Target codecs"] && x.rows["Reasons"] && (!flavour.transcode || /fps/.test(x.rows["Progress / speed"] ?? ""))));
  const info = sections.find((x) => x.title === flavour.panel);
  // innerText collapses jellyfin-web's double spaces.
  const codecs = info.rows["Target codecs"].replace(/\s+/g, " ");
  if (!flavour.codecs.test(codecs)) throw new Error(`Target codecs reads ${JSON.stringify(codecs)}`);
  if (flavour.transcode && (!/^\d+(\.\d)?%/.test(info.rows["Progress / speed"]) || !/ fps \(\d+\.\d\dx\)$/.test(info.rows["Progress / speed"]))) throw new Error(`Progress / speed reads ${JSON.stringify(info.rows["Progress / speed"])}`);
  if (!flavour.transcode && info.rows["Progress / speed"] !== undefined) throw new Error(`a copy shows Progress / speed: ${JSON.stringify(info.rows["Progress / speed"])}`);
  for (const reason of reasons) if (!info.rows["Reasons"].includes(reasonTexts[reason])) throw new Error(`Reasons reads ${JSON.stringify(info.rows["Reasons"])}, lacking ${reason}`);

  const dialog = await dashboardDialog(flavour.film);
  const wanted = flavour.transcode ? [flavour.method, "H264", "AAC", "MP4", "Reason for transcoding", ...reasons.map((r) => reasonTexts[r])] : [flavour.method];
  for (const text of wanted) if (!dialog.includes(text)) throw new Error(`the dashboard card says ${JSON.stringify(dialog)}, lacking ${JSON.stringify(text)}`);
  if (flavour.transcode && (!/Framerate: \d+fps/.test(dialog) || !/\d+(\.\d Mbps| Kbps)/.test(dialog))) throw new Error(`the dashboard card has no frame rate or bitrate: ${JSON.stringify(dialog)}`);
  await leave(page);
});

await scenario("the dashboard card and the Playback info panel for a direct play show no transcode", async ({ page, seen }) => {
  sql("UPDATE sessions SET item_id = NULL, play_method = NULL, play_session_id = NULL");
  await play(page, "Transcode", "Nu Film");
  if (seen.infos.at(-1).source.TranscodingUrl !== undefined) throw new Error("Nu Film should direct play");
  await advances(page);
  const dialog = await dashboardDialog("Nu Film");
  if (!dialog.includes("Direct playing") || /Transcoding|Reason for transcoding/.test(dialog)) throw new Error(`the dashboard card says ${JSON.stringify(dialog)}`);
  await leave(page);
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

// Both streams copied (compat/captures/copy-both-12.4.md). Rho Film is H.264 with
// AC3 (the default track, which Chrome cannot play, so only the video is copied)
// and AAC (the second, which it can: the viewer's pick of it is a remux of
// both). Played, sought far (a job starts in the middle) and read whole as
// another viewer would, with some segments from jobs that began in the middle:
// the picture and the sound in the stream are the file's own packets.
await scenario("copy both: picking the AAC track of an H.264 and AC3 film copies the picture and the sound, through a seek", async ({ page, seen }) => {
  const file = `${media}/Rho Film (2041)/Rho Film (2041).mkv`;
  await play(page, "Transcode", "Rho Film");
  // The scenario "copy, POSIX, Matroska" may have stored this spec already, so
  // the first play can start no ffmpeg at all; that scenario checks its sound is AAC.
  const before = ffmpegRuns();
  if (before.length > 1 || (before.length === 1 && (!before[0].copy || / -c:a copy /.test(before[0].line) || !/-c:a aac/.test(before[0].line)))) {
    throw new Error(`the AC3 track should be encoded with the video copied: ${before.map((r) => r.line.slice(0, 500))}`);
  }
  await page.mouse.move(300, 300);
  await page.mouse.move(600, 400);
  await page.click(".btnAudio");
  await page.click('[data-id="2"]');
  const info = await waitFor("a PlaybackInfo for track 2", async () => seen.infos.find((i) => Number(i.request.AudioStreamIndex) === 2));
  const url = info.source.TranscodingUrl;
  if (!/VideoCodec=copy/.test(url)) throw new Error(`the video is not copied: ${url}`);
  const master = await (await fetch(new URL(url, base))).text();
  if (!/CODECS="avc1\.[0-9A-F]+,mp4a\.40\.2"/i.test(master)) throw new Error(`master: ${master}`);
  await waitFor("segments of the second track", async () => seen.hls.some((r) => r.query.get("AudioStreamIndex") === "2" && /\/hls1\/main\/\d+\.mp4$/i.test(r.path) && r.status === 200));
  await advances(page, 2);
  const copyingBoth = (run) => run.copy && / -c:a copy /.test(run.line) && !/-c:a aac/.test(run.line) && !/libx264/.test(run.line) && run.audio === "2" && run.inputs === 1; // The film is opened once.
  const second = await waitFor("only the second track's ffmpeg to be left", async () => {
    const runs = ffmpegRuns();
    return runs.length === 1 && runs[0].audio === "2" ? runs[0] : null;
  }, 20000);
  if (!copyingBoth(second)) throw new Error(`ffmpeg is not copying both streams: ${second.line.slice(0, 600)}`);
  noErrors(seen);

  // A far seek starts a job in the middle, which copies both as well.
  const total = await duration(page);
  const target = total * 0.7;
  await page.evaluate((t) => { document.querySelector("video").currentTime = t; }, target);
  await waitFor("playback past the seek target", async () => (await time(page)) > target + 1.5, 60000);
  const runs = ffmpegRuns();
  if (runs.length !== 1 || !copyingBoth(runs[0])) throw new Error(`after the seek: ${runs.map((r) => r.line.slice(0, 600))}`);
  noErrors(seen);

  // Every segment, some from jobs that began in the middle: the file's video
  // and the file's second track, packet for packet.
  const playlist = await playlistOf(url);
  const downloader = (u) => { u.searchParams.set("PlaySessionId", randomBytes(16).toString("hex")); return u; };
  const init = await bytesOf(downloader(playlist.init));
  const bodies = [];
  for (const segment of playlist.segments) bodies.push(await bytesOf(downloader(segment.url)));
  const dir = mkdtempSync(`${tmpdir()}/jellyscale-e2e-`);
  writeFileSync(`${dir}/all.mp4`, Buffer.concat([init, ...bodies]));
  const video = (input) => execFileSync("ffmpeg", ["-v", "error", "-i", input, "-map", "0:v:0", "-c", "copy", "-f", "md5", "-"]).toString().trim();
  const got = audioPacketsOf(`${dir}/all.mp4`, "a:0");
  const codec = execFileSync("ffprobe", ["-v", "error", "-select_streams", "a:0", "-show_entries", "stream=codec_name", "-of", "csv=p=0", `${dir}/all.mp4`]).toString().trim();
  const copiedVideo = video(`${dir}/all.mp4`);
  rmSync(dir, { recursive: true, force: true });
  if (copiedVideo !== video(file)) throw new Error(`the video is not the file's: ${copiedVideo} against ${video(file)}`);
  const original = audioPacketsOf(file, "a:1");
  if (!codec.startsWith("aac")) throw new Error(`the sound is ${codec}`);
  // By time, since a tone repeats its packets; the stream's clock is the file's plus 100 s.
  const at = original.findIndex((packet) => Math.abs(packet.time - (got[0].time - 100)) < 0.01);
  if (at < 0 || at > 1 || original.length - got.length > 3 || got.some((packet, i) => packet.data !== original[at + i]?.data || Math.abs(packet.time - 100 - original[at + i].time) > 0.002)) {
    throw new Error(`the sound is not the file's second track (${got.length} packets of ${original.length}, first at ${at})`);
  }
  console.log(`      ${bodies.length} segments: the picture and the sound are the file's own`);

  // Played through the seams, now that every segment exists.
  await page.evaluate(() => { document.querySelector("video").currentTime = 0; });
  await waitFor("playback from the start", async () => (await time(page)) < 2 && (await time(page)) > 0.2, 30000);
  await page.evaluate(() => { window.__waits = 0; document.querySelector("video").addEventListener("waiting", () => window.__waits++); });
  await waitFor("playback to pass 15 s, over two seams", async () => (await time(page)) > 15, 40000);
  const health = await page.evaluate(() => {
    const element = document.querySelector("video");
    return { waits: window.__waits, error: element.error?.message ?? null, corrupted: element.getVideoPlaybackQuality().corruptedVideoFrames };
  });
  if (health.waits || health.error || health.corrupted) throw new Error(`playback through the seams: ${JSON.stringify(health)}`);
  noErrors(seen);
  await leave(page);
});

// Chapters. In a library with chapter pictures off, as Jellyfin has it by
// default, the details page lists none (compat/captures/chapters-12.4.md); in
// one with them on it lists each with its picture, and picking one starts the
// film there. Either way the player knows the chapters: its seek bar has a
// marker for each and its next and previous chapter buttons show. Next chapter
// seeks to the next one's start, which the transcodes' clock (the file's, 2 s
// late in these two) leaves where the file says.
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
// The id is the details page's: once the film plays, the page's URL has none,
// and the session's NowPlayingItem can lag the player.
const detailsId = (page) => page.evaluate(() => new URLSearchParams(location.hash.split("?")[1]).get("id"));
const itemChapters = (page, id) => page.evaluate(async (id) => {
  const client = window.ApiClient;
  return (await client.getItem(client.getCurrentUserId(), id)).Chapters;
}, id);
async function chapterSeek({ page, seen }, library, title, how, images) {
  const imageRequests = [], imageResponses = [];
  page.on("request", (r) => { if (/\/Images\/Chapter\//i.test(r.url())) imageRequests.push(r.url()); });
  page.on("response", (r) => { if (/\/Images\/Chapter\//i.test(r.url())) imageResponses.push({ url: r.url(), status: r.status(), type: r.headers()["content-type"] }); });
  await page.goto(`${base}/web/`);
  await page.waitForSelector(".card", { timeout: 30000 });
  await page.locator(".card:visible", { hasText: library }).first().click();
  await page.locator(".cardText a:visible", { hasText: title }).first().click();
  await page.locator(".btnPlay:visible").first().waitFor({ timeout: 30000 });
  const id = await detailsId(page);
  if (images) {
    // A worker makes the pictures after the scan; the page lists them once it is loaded again.
    await waitFor("the chapters to have pictures", async () => (await itemChapters(page, id)).every((c) => c.ImageTag), 90000);
    await page.reload();
    await page.locator(".btnPlay:visible").first().waitFor({ timeout: 30000 });
  }
  await page.waitForTimeout(1500);
  const hidden = await page.evaluate(() => document.querySelector("#scenesCollapsible")?.classList.contains("hide") ?? true);
  const cards = await page.locator("#scenesContent .chapterCard").count();
  if (!images && !hidden) throw new Error("the details page lists chapters without any picture to show");
  if (images && (hidden || cards !== chapters.length)) throw new Error(`the details page lists ${cards} chapters, hidden: ${hidden}; want ${chapters.length}`);
  if (images) {
    await waitFor("the three pictures", async () => imageResponses.length >= chapters.length, 30000);
    for (const r of imageResponses) if (r.status !== 200 || r.type !== "image/jpeg") throw new Error(`chapter picture ${r.url}: ${r.status} ${r.type}`);
    const asked = new Set(imageRequests.map((u) => /Chapter\/(\d+)/i.exec(u)[1]));
    if (asked.size !== chapters.length) throw new Error(`the page asked for pictures of chapters ${[...asked]}`);
  }

  // Picking the second chapter's card starts the film there.
  const first = images ? 1 : 0;
  const began = Date.now();
  if (images) await page.locator("#scenesContent .chapterCard").nth(first).click();
  else await page.locator(".btnPlay:visible").first().click();
  await waitFor("video to start", async () => (await time(page)) > 0.5, 60000);
  if (images) {
    const at = await time(page), start = chapters[first][1];
    if (at < start - 0.5 || at > start + 8) throw new Error(`picking "${chapters[first][0]}" started the film at ${at} s, not ${start} s`);
    if (how !== "direct") {
      const segment = await waitFor(`a segment at ${start} s`, async () => seen.hls.find((r) => r.at > began && r.status === 200 && /\/\d+\.mp4$/i.test(r.path)
        && Math.abs(Number(r.path.match(/\/(\d+)\.mp4$/i)[1]) - Math.floor(start / 4)) <= 1), 60000);
      console.log(`      picked ${chapters[first][0]}: segment ${segment.path.match(/\/(\d+)\.mp4$/i)[1]} for ${start} s`);
    }
  }
  const source = seen.infos.at(-1).source;
  if (how === "direct" && (source.SupportsDirectPlay !== true || source.TranscodingUrl)) throw new Error(`not direct play: ${JSON.stringify(source).slice(0, 300)}`);
  if (how === "copy" && !/VideoCodec=copy/.test(source.TranscodingUrl ?? "")) throw new Error(`not a copy: ${source.TranscodingUrl}`);
  if (how === "encode" && !/VideoCodec=h264/i.test(source.TranscodingUrl ?? "")) throw new Error(`not re-encoded: ${source.TranscodingUrl}`);

  // What the player was given: the chapters, named as the file and Jellyfin do,
  // with a picture's tag only where pictures are on.
  const given = await itemChapters(page, id);
  if (JSON.stringify(given.map((c) => [c.Name, c.StartPositionTicks / 1e7])) !== JSON.stringify(chapters)) throw new Error(`chapters: ${JSON.stringify(given)}`);
  if (images ? given.some((c) => !/^[0-9a-f]{32}$/.test(c.ImageTag ?? "")) : given.some((c) => "ImageTag" in c)) throw new Error(`the chapters' ImageTags: ${JSON.stringify(given)}`);
  await page.mouse.move(600, 400);
  const osd = await page.evaluate(() => ({
    markers: document.querySelectorAll(".sliderMarkerContainer .sliderMarker").length,
    next: !document.querySelector(".btnNextChapter")?.classList.contains("hide"),
    previous: !document.querySelector(".btnPreviousChapter")?.classList.contains("hide"),
  }));
  if (osd.markers !== chapters.length || !osd.next || !osd.previous) throw new Error(`the player's chapter controls: ${JSON.stringify(osd)}`);

  for (const [name, start] of chapters.slice(first + 1)) {
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
  if (!images && imageRequests.length) throw new Error(`the page asked for chapter images: ${imageRequests.join(" ")}`);
  await leave(page);
}
await scenario("chapters, direct play, Matroska: the player's chapter buttons seek to each chapter", (v) => chapterSeek(v, "Transcode", "Chapters Direct", "direct", false));
await scenario("chapters, direct play, MP4: the player's chapter buttons seek to each chapter", (v) => chapterSeek(v, "Transcode", "Chapters Mp4", "direct", false));
await scenario("chapters, transcode with the video copied and the stream starting late", (v) => chapterSeek(v, "Transcode", "Chapters Copy", "copy", false));
await scenario("chapters, transcode with the video re-encoded and the stream starting late", (v) => chapterSeek(v, "Transcode", "Chapters Encode", "encode", false));
await scenario("chapter pictures, direct play, Matroska: the details page lists the chapters and picking one starts the film there", (v) => chapterSeek(v, "Scenes", "Scenes Direct", "direct", true));
await scenario("chapter pictures, direct play, MP4: the details page lists the chapters and picking one starts the film there", (v) => chapterSeek(v, "Scenes", "Scenes Mp4", "direct", true));
await scenario("chapter pictures, transcode with the video copied and the stream starting late: picking a chapter starts the film there", (v) => chapterSeek(v, "Scenes", "Scenes Copy", "copy", true));
await scenario("chapter pictures, transcode with the video re-encoded and the stream starting late: picking a chapter starts the film there", (v) => chapterSeek(v, "Scenes", "Scenes Encode", "encode", true));

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

// An HDR10 film is copied for a client that plays its video and not its audio
// (JS-025), and CI's Chrome can show that with AV1, which it plays in software
// (HEVC it does not, so the HEVC copy is checked only with E2E_HEVC=1 below).
// The film is AV1 in PQ with AC3 audio: the master says PQ, the video is copied,
// and the player plays and goes on through a seek to a point it has not buffered.
await scenario("copy, POSIX, Matroska: AV1 HDR10 and AC3 plays as PQ with the video copied, through a seek", async ({ page, seen }) => {
  await play(page, "Transcode", "Kaf Film");
  const source = seen.infos.at(-1).source;
  const video = source.MediaStreams.find((s) => s.Type === "Video");
  if (video.Codec !== "av1" || video.VideoRangeType !== "HDR10") throw new Error(`the labels are not an AV1 HDR10 film's: ${JSON.stringify(video)}`);
  const query = new URL(source.TranscodingUrl, base).searchParams;
  if (query.get("VideoCodec") !== "copy" || query.get("TranscodeReasons") !== "AudioCodecNotSupported") throw new Error(`not a copy for the audio alone: ${source.TranscodingUrl}`);
  const answer = await fetch(new URL(source.TranscodingUrl, base));
  const first = (await answer.text()).split("\n").find((line) => line.startsWith("#EXT-X-STREAM-INF"));
  if (!/VIDEO-RANGE=PQ,/.test(first ?? "") || !/CODECS="av01\.0\.\d\dM\.10,/.test(first)) throw new Error(`master: ${first}`);
  const runs = ffmpegRuns();
  if (runs.length !== 1 || !runs[0].copy || /libsvtav1|libx26[45]/.test(runs[0].line)) throw new Error(`ffmpeg is not copying the video: ${runs.map((r) => r.line.slice(0, 400))}`);
  await advances(page, 3);
  // A seek to a point the player has not buffered, so it must fetch a segment
  // there. Not a fixed time: a warm rerun (E2E_REPEAT) resumes where the last
  // run left, and a copy started at 22 s has 24 s buffered within seconds.
  const target = await page.evaluate(() => {
    const ranges = document.querySelector("video").buffered;
    const buffered = (t) => Array.from({ length: ranges.length }, (_, i) => [ranges.start(i), ranges.end(i)]).some(([a, b]) => t >= a && t < b);
    return [24, 4, 12].find((t) => !buffered(t)) ?? null;
  });
  if (target === null) throw new Error("every seek target is buffered already");
  const seeked = Date.now();
  await page.evaluate((t) => { document.querySelector("video").currentTime = t; }, target);
  await waitFor("a segment of the seek", async () => seen.hls.find((r) => r.at > seeked && r.status === 200 && /\/\d+\.mp4$/i.test(r.path)), 60000);
  await waitFor("playback past the seek target", async () => (await time(page)) > target + 2, 60000);
  await advances(page, 2);
  const shown = await page.evaluate(() => ({ frames: document.querySelector("video").getVideoPlaybackQuality().totalVideoFrames, width: document.querySelector("video").videoWidth }));
  if (shown.frames < 20 || shown.width !== 640) throw new Error(`the AV1 picture did not decode: ${JSON.stringify(shown)}`);
  if (!ffmpegRuns().every((r) => r.copy)) throw new Error("ffmpeg stopped copying after the seek");
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
// Matroska and AC3 never direct play; where the browser plays HEVC (JS-025) the
// video is copied as PQ with only the audio re-encoded, and where it does not it
// is the tone-mapped transcode. Its reasons are only checked for the profile without HEVC.
for (const [title, name, reasons, directWithHevc, copiedWithHevc] of [
  ["Resh Film", "HDR10 HEVC in Matroska", "VideoCodecNotSupported,AudioCodecNotSupported", false, true],
  ["Shin Film", "HLG HEVC in MP4", "VideoCodecNotSupported", true, false],
  ["Tav Film", "HDR10 H.264 in Matroska", "VideoRangeTypeNotSupported", false, false],
]) {
  const direct = hevcPlays && directWithHevc;
  const copied = hevcPlays && copiedWithHevc;
  await scenario(`${name} ${direct ? "direct plays (E2E_HEVC=1)" : copied ? "is copied as PQ, only the audio re-encoded (E2E_HEVC=1)" : "plays through a tone-mapped transcode"}`, async ({ page, seen }) => {
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
    if (copied) {
      if (query.get("VideoCodec") !== "copy") throw new Error(`not a copy: ${source.TranscodingUrl}`);
      const first = (await (await fetch(new URL(source.TranscodingUrl, base))).text()).split("\n").find((line) => line.startsWith("#EXT-X-STREAM-INF"));
      if (!/VIDEO-RANGE=PQ,/.test(first) || !/CODECS="hvc1\.2\.4\.L\d+\.B0,/.test(first)) throw new Error(`master: ${first}`);
      const runs = ffmpegRuns();
      if (runs.length !== 1 || !runs[0].copy) throw new Error(`ffmpeg is not copying: ${runs.map((r) => r.line.slice(0, 400))}`);
      await advances(page, 4);
      noErrors(seen);
      await leave(page);
      return;
    }
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

// Dolby Vision (compat/captures/dolby-vision-12.4.md). The item says what Jellyfin's
// labels say, and the label decides. Profile 8.1's base layer is HDR10: a browser
// that plays HEVC HDR10 but lists no Dolby Vision is sent the file's own video with the
// base layer's range and codec string (the Dolby Vision brand as SUPPLEMENTAL-CODECS),
// and one that plays no HEVC gets the tone-mapped H.264. Profile 5 has no base layer
// any such browser can show, so it is re-encoded, never copied. E2E_HEVC says which
// browser this is, as for the films above. Films are in the HDR library (media/hdr).
await scenario("Dolby Vision 8.1 in MP4" + `: ${hevcPlays ? "copied as HDR10 (E2E_HEVC=1)" : "re-encoded to H.264, tone mapped"}`, async ({ page, seen }) => {
  await play(page, "HDR", "Dolby Eight One Film");
  const source = seen.infos.at(-1).source;
  const video = source.MediaStreams.find((s) => s.Type === "Video");
  if (video.VideoRange !== "HDR" || video.VideoRangeType !== "DOVIWithHDR10" || video.DvProfile !== 8 || video.DvBlSignalCompatibilityId !== 1) {
    throw new Error(`the labels are not Jellyfin's: ${JSON.stringify(video)}`);
  }
  const query = new URL(source.TranscodingUrl, base).searchParams;
  const runs = ffmpegRuns();
  if (hevcPlays) {
    if (query.get("VideoCodec") !== "copy" || query.get("TranscodeReasons") !== "VideoRangeTypeNotSupported") throw new Error(`not a copy: ${source.TranscodingUrl}`);
    const first = (await (await fetch(new URL(source.TranscodingUrl, base))).text()).split("\n").find((line) => line.startsWith("#EXT-X-STREAM-INF"));
    if (!/VIDEO-RANGE=PQ,/.test(first) || !/CODECS="hvc1\.2\.4\.L\d+\.B0,/.test(first) || !/SUPPLEMENTAL-CODECS="dvh1\.08\.10\/db1p"/.test(first)) throw new Error(`master: ${first}`);
    if (runs.length !== 1 || !runs[0].copy) throw new Error(`ffmpeg is not copying: ${runs.map((r) => r.line.slice(0, 400))}`);
  } else {
    if (query.get("VideoCodec") !== "h264") throw new Error(`not a re-encode: ${source.TranscodingUrl}`);
    if (runs.length !== 1 || runs[0].copy || !/tonemap=tonemap=hable/.test(runs[0].line)) throw new Error(`ffmpeg is not tone mapping: ${runs.map((r) => r.line.slice(0, 400))}`);
  }
  await advances(page, 4);
  noErrors(seen);
  await leave(page);
});

await scenario("Dolby Vision 5 in MP4" + `: re-encoded to H.264, never copied${hevcPlays ? " (E2E_HEVC=1)" : ""}`, async ({ page, seen }) => {
  await play(page, "HDR", "Dolby Five Film");
  const source = seen.infos.at(-1).source;
  const video = source.MediaStreams.find((s) => s.Type === "Video");
  if (video.VideoRange !== "HDR" || video.VideoRangeType !== "DOVI" || video.DvProfile !== 5) throw new Error(`the labels are not Jellyfin's: ${JSON.stringify(video)}`);
  const query = new URL(source.TranscodingUrl, base).searchParams;
  if (query.get("VideoCodec") !== "h264") throw new Error(`profile 5 was not re-encoded: ${source.TranscodingUrl}`);
  if (hevcPlays && query.get("TranscodeReasons") !== "VideoRangeTypeNotSupported") throw new Error(`reasons ${query.get("TranscodeReasons")}`);
  const runs = ffmpegRuns();
  if (runs.length !== 1 || runs[0].copy) throw new Error(`ffmpeg: ${runs.map((r) => r.line.slice(0, 400))}`);
  await advances(page, 4);
  noErrors(seen);
  await leave(page);
});

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
// Every ffmpeg command line the worker has run, from the shim's log beside the server's.
const ffmpegLines = () => readFileSync(`${dirname(process.env.E2E_LOG)}/ffmpeg.log`, "utf8").split("\n").filter(Boolean);

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
  let logged = ffmpegLines().length;
  await cpuPerSegment(page, seen, "Stigma Film", segments, null);
  const copy = workerCpu() - before;
  // What a copy does, not what it costs: every ffmpeg that made its picture copied it.
  const copied = ffmpegLines().slice(logged).filter((line) => /-hls_time/.test(line) && /-c:v /.test(line));
  if (!copied.length || copied.some((line) => !/-c:v copy/.test(line))) throw new Error(`the copy's ffmpeg did not copy the picture: ${JSON.stringify(copied.map((l) => l.slice(0, 200)))}`);
  const copyRuns = seen.infos.at(-1).source.TranscodingUrl;
  if (!/VideoCodec=copy/.test(copyRuns)) throw new Error("the first run was not a copy");
  const second = await viewer();
  try {
    before = workerCpu();
    logged = ffmpegLines().length;
    await cpuPerSegment(second.page, second.seen, "Stigma Film", segments, 3000000);
    const encoded = workerCpu() - before;
    if (!/VideoCodec=h264/.test(second.seen.infos.at(-1).source.TranscodingUrl)) throw new Error("the second run was not a re-encode");
    // Logged, not judged: CPU seconds depend on the machine. The re-encode is the run that encoded the picture.
    console.log(`      1080p, ${segments * 4} s of film: copy ${copy.toFixed(2)} CPU s, re-encode ${encoded.toFixed(2)} CPU s (${(copy / encoded * 100).toFixed(0)}%)`);
    const encodedLines = ffmpegLines().slice(logged).filter((line) => /-hls_time/.test(line) && /-c:v /.test(line));
    if (!encodedLines.length || encodedLines.some((line) => !/-c:v libx264/.test(line))) throw new Error(`the re-encode's ffmpeg did not encode the picture: ${JSON.stringify(encodedLines.map((l) => l.slice(0, 200)))}`);
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

// ---- How far a job runs ahead of the viewer ----
// The config sets transcodeLead to 20 s, 5 segments. The job stops its ffmpeg
// (SIGSTOP) once it has made that much beyond the furthest segment the player
// asked for, and goes on as the player asks for more.
function stoppedFfmpeg() {
  const runs = ffmpegRuns();
  if (runs.length !== 1) return false;
  const pid = runs[0].line.split(" ")[0];
  try {
    return readFileSync(`/proc/${pid}/stat`, "utf8").replace(/^.*\) /, "")[0] === "T";
  } catch {
    return false;
  }
}

for (const flavour of [{ name: "copied", film: "Lead Copy Film" }, { name: "re-encoded", film: "Lead Encode Film" }]) await scenario("the job stops its ffmpeg five segments past what the player asked for, and the film still plays and seeks" + `: ${flavour.name}`, async ({ page, seen }) => {
  await play(page, "Transcode", flavour.film);
  await advances(page, 4);
  await waitFor("ffmpeg to stop at its lead", async () => stoppedFfmpeg(), 60000);
  const segmentOf = (r) => Number(r.path.match(/\/(\d+)\.mp4$/i)?.[1]);
  const next = Number(sql("SELECT max(next_segment) FROM transcode_jobs WHERE state = 'running'"));
  const asked = Math.max(...seen.hls.filter((r) => r.status === 200 && Number.isFinite(segmentOf(r))).map(segmentOf));
  const count = Math.ceil((await duration(page)) / 4);
  console.log(`      the job stored ${next} segments, the player asked for up to ${asked}, the film has ${count}`);
  if (next > asked + 5) throw new Error(`the job stored ${next} segments, more than 5 past the ${asked} the player asked for`);
  if (next >= count - 1) throw new Error("the job ran to the end of the film, so nothing was bounded");
  // It goes on as the player plays, and a seek past the lead restarts the job there.
  const played = await time(page);
  await waitFor("playback to go on through several leads", async () => (await time(page)) > played + 25, 90000);
  const total = await duration(page);
  const target = total * 0.8;
  await page.evaluate((t) => { document.querySelector("video").currentTime = t; }, target);
  await waitFor("playback past the seek target", async () => (await time(page)) > target + 1.5, 60000);
  noErrors(seen);
  await leave(page);
  await waitFor("ffmpeg to exit after Stopped", async () => ffmpegRuns().length === 0, 20000);
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
    const previous = seen.infos.at(-1).session;
    const deletes = () => seen.deletes.filter((url) => url.includes(previous)).length;
    const before = deletes();
    await page.mouse.move(600, 400);
    await page.click(".btnVideoOsdSettings");
    await page.click('[data-id="quality"]');
    await page.click(`[data-id="${id}"]`);
    const info = await waitFor(`a PlaybackInfo with the cap ${id}`, async () => seen.infos.find((i) => i.request.MaxStreamingBitrate === Number(id)));
    // jellyfin-web ends the old encoding once as it changes the stream and again
    // when the new one plays; until then it sends no Stopped on leaving.
    await waitFor("the player to finish changing the stream", async () => deletes() >= before + 2, 30000);
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
// The scenario's own viewer is the second one's browser context, so it opens one more.
await scenario("two viewers share a job and stopping one leaves the other playing", async (a) => {
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
  } finally {
    await b.context.close().catch(() => {});
  }
});

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

// JS-029: Postgres is stopped and started again (docker stop and start: every
// connection ends, and new ones are refused until it is back, which is what a
// restart or a switchover does to the replicas and the worker) while a film
// plays. The container keeps its host port and its data across the stop.
const exec = promisify(execFile);
async function postgres(action) {
  await exec("docker", [action, process.env.E2E_PG]);
  if (action === "start") {
    await waitFor("Postgres to answer again", async () => {
      try { await exec("docker", ["exec", process.env.E2E_PG, "psql", "-U", "postgres", "-h", "127.0.0.1", "-Atc", "select 1"]); return true; } catch { return false; }
    }, 60000);
  }
}

// Stops Postgres, runs during() if given, holds it away until the browser has made
// at least one request that matches (and three seconds more, so that the replicas
// have waited on it), then starts it again. It returns the requests the browser made
// while Postgres was away.
async function awayUntil(seen, matches, during) {
  const downAt = clock();
  await postgres("stop");
  try {
    if (during) await during();
    await waitFor("the browser to ask for something while Postgres is away", async () => seen.requests.some((r) => r.at > downAt && matches(r)), 60000);
    await new Promise((resolve) => setTimeout(resolve, 3000));
  } finally {
    await postgres("start");
  }
  return seen.requests.filter((r) => r.at > downAt && matches(r));
}

// Nothing the browser asked for, of either role, was answered with an error.
function noAnswerOf400OrMore(seen) {
  const bad = seen.answers.filter((a) => a.status >= 400);
  if (bad.length) throw new Error(`answers of 400 or more: ${JSON.stringify(bad.slice(0, 4).map((a) => `${a.status} ${a.method} ${new URL(a.url).pathname} from ${a.routed}`))}`);
}

await scenario("Postgres restarted in the middle of a transcode: the same player goes on past where it was, with no dialog and no new PlaybackInfo", async ({ page, seen }) => {
  await play(page, "Transcode", "Outage Encode Film");
  await advances(page, 2);
  if (seen.infos.at(-1).source.TranscodingSubProtocol !== "hls") throw new Error("the film should be an HLS transcode");
  await page.evaluate(() => { window.__outageVideo = document.querySelector("video"); });
  const asked = seen.infos.length;
  const reached = await time(page);
  const segments = (r) => /\/hls1\//i.test(r.url) && !/m3u8/i.test(r.url);
  const whileAway = await awayUntil(seen, segments);
  console.log(`      the browser asked for ${whileAway.length} segments while Postgres was away (${whileAway.filter((r) => r.method === "GET").length} GETs)`);
  const now = await time(page);
  await waitFor(`the picture to pass ${Math.max(reached, now) + 3}`, async () => (await time(page)) > Math.max(reached, now) + 3, 90000);
  if (await page.getByText("Playback Error").count()) throw new Error("the viewer got a Playback Error dialog");
  if (!(await page.evaluate(() => document.querySelector("video") === window.__outageVideo))) throw new Error("the player was replaced");
  if (seen.infos.length !== asked) throw new Error(`the client asked for PlaybackInfo again (${seen.infos.length} times, not ${asked})`);
  noAnswerOf400OrMore(seen);
  await leave(page);
  await waitFor("ffmpeg to exit after Stopped", async () => ffmpegRuns().length === 0, 30000);
});

await scenario("Postgres restarted while a film direct plays: a seek to what is not buffered is answered, with no dialog", async ({ page, seen }) => {
  await play(page, "Transcode", "Outage Direct Film");
  if (seen.infos.at(-1).source.TranscodingUrl !== undefined) throw new Error("the film should direct play");
  await advances(page, 1);
  const target = (await duration(page)) * 0.8;
  // The seek is to a place the browser has not got, so it must ask the stream replica for a range.
  const whileAway = await awayUntil(seen, (r) => /\/stream/i.test(r.url) && r.method === "GET", () => page.evaluate((to) => { document.querySelector("video").currentTime = to; }, target));
  console.log(`      the browser asked for ${whileAway.length} ranges while Postgres was away`);
  await waitFor(`the picture to play on from ${target.toFixed(0)} s`, async () => (await time(page)) > target + 1, 90000);
  if (await page.getByText("Playback Error").count()) throw new Error("the viewer got a Playback Error dialog");
  noAnswerOf400OrMore(seen);
  await leave(page);
});

// JS-085: with Postgres stopped, a stream replica goes on serving the token of a recently active playback session. The film starts, Postgres is stopped (docker stop, as above), and the viewer seeks to a place
// the browser does not have: a range of the file for a direct play, segments in the transcode store for an HLS
// transcode. The picture must play on from there while Postgres is still stopped, on the same player, with no dialog,
// no new PlaybackInfo and no answer of 400 or more to anything the browser asked. The scenarios check that Postgres is
// still stopped when they look, and say how many ranges or segments were answered meanwhile.
const routerControl = async (body) => (await fetch(`${home}/__router`, body ? { method: "PUT", body: JSON.stringify(body) } : {})).json();

async function postgresRunning() {
  return (await exec("docker", ["inspect", "-f", "{{.State.Running}}", process.env.E2E_PG])).stdout.trim() === "true";
}

const bufferedAt = (page, at) => page.evaluate((t) => {
  const ranges = document.querySelector("video").buffered;
  return Array.from({ length: ranges.length }, (_, i) => [ranges.start(i), ranges.end(i)]).some(([a, b]) => t >= a - 0.1 && t <= b + 0.1);
}, at);

// Where the player's buffer ends, from the current time on.
const bufferEnd = (page) => page.evaluate(() => {
  const v = document.querySelector("video");
  for (let i = 0; i < v.buffered.length; i++) if (v.buffered.start(i) <= v.currentTime + 0.5 && v.buffered.end(i) >= v.currentTime) return v.buffered.end(i);
  return v.currentTime;
});

// Requests that need Postgres from each replica of both roles, which find it away at once and so, 30 seconds later,
// fail their readiness. They get no answer before Postgres is back; the signal ends them.
function findPostgresAway(signal) {
  const asked = [];
  for (let i = 0; i < 8; i++) {
    const headers = { authorization: 'MediaBrowser Token="not-a-token"' };
    asked.push(fetch(`${home}/Users/Me`, { headers, signal }).catch(() => {}));
    asked.push(fetch(`${home}/Videos/00000000000000000000000000000001/stream?ApiKey=not-a-token`, { headers, signal }).catch(() => {}));
  }
  return asked;
}

// The state every scenario below checks at the end, with Postgres still stopped.
async function playedOnWhileStopped({ page, seen }, { asked, downAt, target, answered }) {
  if (await postgresRunning()) throw new Error("Postgres was running again before the checks: the film did not play on from its own replica");
  if (await page.getByText("Playback Error").count()) throw new Error("the viewer got a Playback Error dialog");
  if (!(await page.evaluate(() => document.querySelector("video") === window.__awayVideo))) throw new Error("the player was replaced");
  if (seen.infos.length !== asked) throw new Error(`the client asked for PlaybackInfo again (${seen.infos.length} times, not ${asked})`);
  const meanwhile = seen.answers.filter((a) => a.at > downAt && answered(a));
  if (!meanwhile.length) throw new Error("nothing the player asked for after Postgres stopped was answered as it should be");
  console.log(`      ${meanwhile.length} answers came from the replica while Postgres was stopped, the picture was at ${(await time(page)).toFixed(1)} s after a seek to ${target.toFixed(1)} s`);
  noAnswerOf400OrMore(seen);
}

// Every stream replica refreshes a snapshot of the tokens of recently active playback sessions and serves a token in
// it, while Postgres is away, for direct play bytes and stored segments (JS-085). So the film is played through one
// stream replica only, and the seek during the outage reaches the other, which never saw the viewer: the router
// sends to one replica at a time, which no ingress setting can make a build depend on. The snapshot is refreshed
// every few seconds (DOMAINS 37 says how many, at most 10), so the scenario waits twice that after the player's
// first reports before it stops Postgres; jellyfin-web reports the start and then every ten seconds, so the
// session is recent for the whole outage's first minutes at least.
const SNAPSHOT_REFRESHED = 22000;

// Starts a second stream replica, B, and hands the body the ports of A (the one the stack started) and B. The router
// sends the stream role to A alone until the body says otherwise.
async function withTwoStreamReplicas(body) {
  const port = await new Promise((resolve) => {
    const probe = createServer();
    probe.listen(0, "127.0.0.1", () => { const { port } = probe.address(); probe.close(() => resolve(port)); });
  });
  const child = spawn(process.env.E2E_STREAM_REPLICA, [String(port)], { stdio: "ignore" });
  try {
    await waitFor("the second stream replica to answer /healthz", async () => {
      try { return (await fetch(`http://127.0.0.1:${port}/healthz`)).ok; } catch { return false; }
    }, 60000);
    const [a] = (await routerControl()).stream.map((b) => b.port);
    return await body({ a, b: port });
  } finally {
    await routerControl({ reset: true });
    child.kill("SIGKILL");
  }
}

// How an outage scenario ends: Postgres starts, every replica is ready again (the router keeps falling back to every
// replica until then, as the shipped Caddyfile does, and the second replica keeps running), and only then the viewer
// leaves, which ends the requests the player has in flight. The scenarios used to reset the router and kill the second
// replica first, with Postgres still stopped: the response of the range the player was reading from that replica was cut
// (net::ERR_CONTENT_LENGTH_MISMATCH) and the router, its fallback off and every /readyz still 503, answered the next
// range 503 itself, and the player asked for PlaybackInfo again and left the film (JS-085's queue run, seen again in
// JS-103: the answer of 400 or more came from the router with x-routed-to none, the cut from the killed replica, both
// ending at the scenario's own teardown, 32 s after Postgres stopped). Then nothing the browser asked for since the
// outage began may have been cut, refused, or have made it ask for PlaybackInfo again.
async function endOutage({ page, seen }, asked, downAt, started = false) {
  if (!started) await postgres("start");
  await waitFor("every replica to be ready again", async () => {
    const state = await routerControl();
    return [...state.api, ...state.stream].every((x) => x.up);
  }, 60000);
  await leave(page);
  if (seen.infos.length !== asked) throw new Error(`the client asked for PlaybackInfo again (${seen.infos.length} times, not ${asked}): ${page.timeline.slice(-6).join("; ")}`);
  noAnswerOf400OrMore(seen);
  // ERR_ABORTED is the player's own: a seek ends the range it was reading, hls.js gives up on a segment after ten seconds.
  const cut = seen.cut.filter((x) => x.at > downAt && x.error !== "net::ERR_ABORTED");
  if (cut.length) throw new Error(`requests were cut or refused after Postgres stopped: ${JSON.stringify(cut.slice(0, 4).map((x) => `${x.method} ${new URL(x.url).pathname} ${x.error}`))}`);
}

// A direct play: the router slows the film to a download of tens of seconds, so the seek is to bytes the browser does not have.
for (const flavour of [{ name: "within the window of the film starting", film: "Away Direct Film", late: false }, { name: "past readiness, with every replica's /readyz at 503 and the router sending to all of them as the shipped Caddyfile does", film: "Away Late Film", late: true }]) {
  await scenario("Postgres stopped while a film direct plays: a seek to what is not buffered plays on, from another stream replica, while it is still stopped" + `: ${flavour.name}`, async (v) => {
    const { page, seen } = v;
    const probes = new AbortController();
    let stopped = false;
    try {
     await withTwoStreamReplicas(async ({ a, b }) => {
      await routerControl({ throttle: 1_500_000, fallback: true, stream: [a] });
      await play(page, "Transcode", flavour.film);
      if (seen.infos.at(-1).source.TranscodingUrl !== undefined) throw new Error("the film should direct play");
      await advances(page, 1);
      await page.evaluate(() => { window.__awayVideo = document.querySelector("video"); });
      const asked = seen.infos.length;
      const target = (await duration(page)) * 0.8;
      if (await bufferedAt(page, target)) throw new Error(`the browser already has ${target.toFixed(0)} s: the seek would need no request`);
      const before = seen.answers.filter((x) => /\/videos\/[^/]+\/stream/i.test(x.url) && x.backend);
      if (!before.length || before.some((x) => Number(x.backend) !== a)) throw new Error(`the film's ranges before the outage should all come from replica ${a}: ${before.map((x) => x.backend)}`);
      await new Promise((resolve) => setTimeout(resolve, SNAPSHOT_REFRESHED));
      const downAt = clock();
      await postgres("stop");
      stopped = true;
      if (flavour.late) {
        // Both replicas find Postgres away through probes that take turns, and fail readiness 30 seconds on.
        await routerControl({ stream: [a, b] });
        findPostgresAway(probes.signal);
        await waitFor("every replica's /readyz to answer 503", async () => {
          const state = await routerControl();
          return state.stream.length === 2 && [...state.api, ...state.stream].every((x) => !x.up);
        }, 120000);
        console.log(`      every replica failed readiness ${((clock() - downAt) / 1000).toFixed(0)} s after Postgres stopped`);
      }
      // From here the stream role is replica B alone, which never served this viewer.
      await routerControl({ stream: [b] });
      const seekAt = clock();
      await page.evaluate((to) => { document.querySelector("video").currentTime = to; }, target);
      await waitFor("a range of the film to be answered 206 while Postgres is stopped", async () => seen.answers.some((x) => x.at > seekAt && /\/stream/i.test(x.url) && x.status === 206), 60000);
      await waitFor(`the picture to play on from ${target.toFixed(0)} s`, async () => (await time(page)) > target + 1, 60000);
      await playedOnWhileStopped(v, { asked, downAt, target, answered: (x) => /\/stream/i.test(x.url) && x.status === 206 });
      const after = seen.answers.filter((x) => x.at > seekAt && /\/videos\/[^/]+\/stream/i.test(x.url) && x.status === 206 && x.backend);
      if (!after.length || after.some((x) => Number(x.backend) !== b)) throw new Error(`the ranges after the seek should come from replica ${b}, which never served the viewer: ${after.map((x) => x.backend)}`);
      probes.abort();
      stopped = false;
      await endOutage(v, asked, downAt);
     });
    } finally {
      probes.abort();
      if (stopped) await postgres("start");
      await routerControl({ reset: true });
      await waitFor("every replica to be ready again", async () => {
        const state = await routerControl();
        return [...state.api, ...state.stream].every((x) => x.up);
      }, 60000);
    }
  });
}

// HLS: the first seconds of the film are made and stored, the player has fetched what its buffer holds, and the seek is to
// segments the job stored beyond that, past the 30 seconds after which readiness fails, asked of a replica that never saw
// the viewer. The scenario takes Postgres down only when the store holds a segment past the buffer.
await scenario("Postgres stopped while a film plays as an HLS transcode: a seek to segments in the transcode store that the player has not buffered plays on, from another stream replica, while it is still stopped", async (v) => {
  const { page, seen } = v;
  const probes = new AbortController();
  let stopped = false;
  try {
   await withTwoStreamReplicas(async ({ a, b }) => {
    await routerControl({ fallback: true, stream: [a] });
    await play(page, "Transcode", "Away Encode Film");
    if (seen.infos.at(-1).source.TranscodingSubProtocol !== "hls") throw new Error("the film should be an HLS transcode");
    await advances(page, 1);
    await page.evaluate(() => { window.__awayVideo = document.querySelector("video"); document.querySelector("video").pause(); });
    const asked = seen.infos.length;
    const session = seen.infos.at(-1).session;
    const storedEnd = () => 4 * Number(sql(`SELECT COALESCE(max(next_segment), 0) FROM transcode_jobs WHERE owner = '${session}'`));
    // The job runs on until its lead, past what the paused player asks for.
    await waitFor("segments in the store past the player's buffer", async () => storedEnd() >= (await bufferEnd(page)) + 8, 90000);
    const end = await bufferEnd(page);
    const m = Math.floor(end / 4) + 1;
    const target = 4 * m + 0.5;
    if (await bufferedAt(page, target) || storedEnd() < 4 * (m + 1)) throw new Error(`the target ${target} s is buffered, or its segment is not stored (stored to ${storedEnd()} s, buffered to ${end} s)`);
    console.log(`      the store holds the film to ${storedEnd()} s, the player's buffer ends at ${end.toFixed(1)} s, the seek is to ${target} s (segment ${m})`);
    await new Promise((resolve) => setTimeout(resolve, SNAPSHOT_REFRESHED));
    const downAt = clock();
    await postgres("stop");
    stopped = true;
    // Past readiness: both replicas find Postgres away and answer 503 on /readyz; the router then sends to them as the shipped Caddyfile does.
    await routerControl({ stream: [a, b] });
    findPostgresAway(probes.signal);
    await waitFor("every replica's /readyz to answer 503", async () => {
      const state = await routerControl();
      return state.stream.length === 2 && [...state.api, ...state.stream].every((x) => !x.up);
    }, 120000);
    await routerControl({ stream: [b] });
    const seekAt = clock();
    await page.evaluate((to) => { const video = document.querySelector("video"); video.currentTime = to; video.play(); }, target);
    await waitFor(`segment ${m} to be answered 200 while Postgres is stopped`, async () => seen.hls.find((r) => r.at > seekAt && r.status === 200 && r.path.endsWith(`/${m}.mp4`)), 60000);
    await waitFor(`the picture to play on from ${target} s`, async () => (await time(page)) > target + 1, 60000);
    await playedOnWhileStopped(v, { asked, downAt, target, answered: (x) => /\/hls1\//i.test(x.url) && /\/\d+\.mp4/i.test(x.url) && x.status === 200 });
    const after = seen.answers.filter((x) => x.at > seekAt && /\/hls1\//i.test(x.url) && /\/\d+\.mp4/i.test(x.url) && x.status === 200 && x.backend);
    if (!after.length || after.some((x) => Number(x.backend) !== b)) throw new Error(`the segments after the seek should come from replica ${b}, which never served the viewer: ${after.map((x) => x.backend)}`);
    probes.abort();
    stopped = false;
    await endOutage(v, asked, downAt);
   });
  } finally {
    probes.abort();
    if (stopped) await postgres("start");
    await routerControl({ reset: true });
    await waitFor("every replica to be ready again", async () => {
      const state = await routerControl();
      return [...state.api, ...state.stream].every((x) => x.up);
    }, 60000);
  }
  await waitFor("ffmpeg to exit after Stopped", async () => ffmpegRuns().length === 0, 30000);
});

// JS-103: Postgres answers again after an outage longer than 60 s (the window of JS-085, and transcodeIdle) and past
// the readiness cut (30 s). The film that played on through it must play on, with the ingress doing what the shipped
// Caddyfile does throughout: /readyz of a replica is 503 until it has seen Postgres answer, and until then the
// ingress sends to every replica (fallback stays on for the whole scenario, not reset when Postgres is started).
// The film is not left, no dialog, no new PlaybackInfo, no answer of 400 or more, no request cut, and a request in
// flight across the return is neither cut nor answered with an error. A seek after the return to bytes not
// buffered is answered.
// How long Postgres is away: past 60 s (JS-085's window, and transcodeIdle), with a margin. DOMAINS 37's window is longer now,
// so that what this shows is the return itself and not the window ending.
const LONG_OUTAGE = 75000;
const AFTER_RETURN = 35000;

const streamUrl = (request) => /\/videos\/[^/]+\/stream/i.test(request.url());

// The same two ways a film plays: a direct play, with a range of the film in flight across the return, and an HLS
// transcode, whose store holds the film only as far as its job had made it when Postgres stopped (a worker stores
// nothing while Postgres is away), so the picture stalls at the end of the store until Postgres is back and then
// has to go past it, with a job that nobody recorded a request for in over transcodeIdle (60 s).
for (const flavour of [{ name: "direct play", film: "Return Direct Film", hls: false }, { name: "HLS transcode", film: "Return Encode Film", hls: true }]) {
  await scenario(`Postgres back after an outage longer than a minute and past readiness: a film that played through it as a ${flavour.name} plays on, with the ingress falling back as the shipped Caddyfile does`, async (v) => {
    const { page, seen } = v;
    const probes = new AbortController();
    let stopped = false;
    try {
      await withTwoStreamReplicas(async ({ a, b }) => {
        await routerControl({ throttle: flavour.hls ? 0 : 1_500_000, fallback: true, stream: [a] });
        await play(page, "Transcode", flavour.film);
        if ((seen.infos.at(-1).source.TranscodingSubProtocol === "hls") !== flavour.hls) throw new Error(`the film should ${flavour.hls ? "be an HLS transcode" : "direct play"}`);
        await advances(page, 1);
        await page.evaluate(() => { window.__awayVideo = document.querySelector("video"); });
        const asked = seen.infos.length;
        await new Promise((resolve) => setTimeout(resolve, SNAPSHOT_REFRESHED));
        const downAt = clock();
        await postgres("stop");
        stopped = true;
        await routerControl({ stream: [a, b] });
        findPostgresAway(probes.signal);
        await waitFor("every replica's /readyz to answer 503", async () => {
          const state = await routerControl();
          return state.stream.length === 2 && [...state.api, ...state.stream].every((x) => !x.up);
        }, 120000);
        await waitFor("the outage to outlast a minute", async () => clock() - downAt > LONG_OUTAGE, 120000);
        const inFlight = [...seen.open].filter(streamUrl).length;
        const before = await time(page);
        console.log(`      ${inFlight} range(s) of the film in flight at the return, the picture at ${before.toFixed(1)} s`);
        const backAt = clock();
        await postgres("start");
        stopped = false;
        // The ingress is left as it was: fallback on, whatever /readyz says, until the replicas say ready.
        await waitFor(`the picture to play ${AFTER_RETURN / 1000} s on from the return`, async () => (await time(page)) > before + AFTER_RETURN / 1000, 120000)
          .catch(async (error) => { throw new Error(`${error.message}; the picture is at ${(await time(page)).toFixed(1)} s, the player's last requests: ${page.timeline.slice(-8).join("; ")}; cut: ${JSON.stringify(seen.cut.filter((x) => x.at > downAt).slice(0, 4).map((x) => `${x.method} ${new URL(x.url).pathname} ${x.error}`))}; answers of 400 or more: ${JSON.stringify(seen.answers.filter((x) => x.status >= 400).slice(0, 4).map((x) => `${x.status} ${new URL(x.url).pathname} from ${x.routed}`))}`); });
        if (await page.getByText("Playback Error").count()) throw new Error("the viewer got a Playback Error dialog");
        if (!(await page.evaluate(() => document.querySelector("video") === window.__awayVideo))) throw new Error("the player was replaced");
        if (seen.infos.length !== asked) throw new Error(`the client asked for PlaybackInfo again (${seen.infos.length} times, not ${asked}): ${page.timeline.slice(-6).join("; ")}`);
        // ERR_ABORTED is the player's own (hls.js gives up on a segment that has waited ten seconds for Postgres, a seek
        // ends the range being read): anything else the browser lost is a response cut or refused.
        const cut = seen.cut.filter((x) => x.at > downAt && x.error !== "net::ERR_ABORTED");
        if (cut.length) throw new Error(`requests were cut or refused after Postgres stopped: ${JSON.stringify(cut.slice(0, 4).map((x) => `${x.method} ${new URL(x.url).pathname} ${x.error}`))}`);
        noAnswerOf400OrMore(seen);
        // A seek to what the browser has not got is answered by a replica that has Postgres again.
        const target = (await duration(page)) * 0.85;
        if (await bufferedAt(page, target)) throw new Error(`the browser already has ${target.toFixed(0)} s: the seek would need no request`);
        const seekAt = clock();
        await page.evaluate((to) => { document.querySelector("video").currentTime = to; }, target);
        await waitFor("the seek to be answered", async () => seen.answers.some((x) => x.at > seekAt && (flavour.hls ? /\/hls1\/main\/\d+\.mp4/i.test(x.url) : /\/stream/i.test(x.url)) && x.status < 300), 60000);
        await waitFor(`the picture to play on from ${target.toFixed(0)} s`, async () => (await time(page)) > target + 1, 60000);
        noAnswerOf400OrMore(seen);
        if (seen.infos.length !== asked) throw new Error(`the client asked for PlaybackInfo again after the seek (${seen.infos.length} times, not ${asked})`);
        console.log(`      Postgres was away ${((backAt - downAt) / 1000).toFixed(0)} s; the film played ${AFTER_RETURN / 1000} s and a seek on`);
        await endOutage(v, asked, downAt, true);
      });
    } finally {
      probes.abort();
      if (stopped) await postgres("start");
      await routerControl({ reset: true });
      await waitFor("every replica to be ready again", async () => {
        const state = await routerControl();
        return [...state.api, ...state.stream].every((x) => x.up);
      }, 60000);
    }
    if (flavour.hls) await waitFor("ffmpeg to exit after Stopped", async () => ffmpegRuns().length === 0, 30000);
  });
}

// JS-103: a film whose picture began a second before Postgres stopped plays on. The scenario does not wait for any
// refresh of the snapshot: Postgres is killed (docker kill, which stops it at once; docker stop lets it wait for its
// clients for seconds) as soon as the picture has played a second, which is after jellyfin-web reported the start, and
// the seek to bytes the browser has not got is answered by the replica that served the film and, in the second
// scenario, by one that served the viewer nothing. api and stream are separate processes here, as in production.
// What a refresh every few seconds could hide: the start report reached Postgres a second or more before, so a build
// whose replicas refresh every second passes, and one that refreshes every 5 seconds passes when a refresh happens to
// fall in that second, which internal/api's test makes certain of (justRefreshed) and this one cannot.
for (const flavour of [{ name: "the replica that served it", film: "Just Started Film", elsewhere: false }, { name: "a stream replica that served the viewer nothing", film: "Just Started Other Film", elsewhere: true }]) {
  await scenario(`Postgres killed a second after the picture began: a seek to what is not buffered plays on, from ${flavour.name}, while it is still stopped`, async (v) => {
    const { page, seen } = v;
    let stopped = false;
    try {
      await withTwoStreamReplicas(async ({ a, b }) => {
        await routerControl({ throttle: 1_500_000, fallback: true, stream: [a] });
        await play(page, "Transcode", flavour.film);
        if (seen.infos.at(-1).source.TranscodingUrl !== undefined) throw new Error("the film should direct play");
        await advances(page, 1);
        await page.evaluate(() => { window.__awayVideo = document.querySelector("video"); });
        const asked = seen.infos.length;
        const target = (await duration(page)) * 0.8;
        if (await bufferedAt(page, target)) throw new Error(`the browser already has ${target.toFixed(0)} s: the seek would need no request`);
        const downAt = clock();
        await postgres("kill");
        stopped = true;
        await routerControl({ stream: [flavour.elsewhere ? b : a] });
        const seekAt = clock();
        await page.evaluate((to) => { document.querySelector("video").currentTime = to; }, target);
        await waitFor("a range of the film to be answered 206 while Postgres is stopped", async () => seen.answers.some((x) => x.at > seekAt && /\/stream/i.test(x.url) && x.status === 206), 60000);
        await waitFor(`the picture to play on from ${target.toFixed(0)} s`, async () => (await time(page)) > target + 1, 60000);
        await playedOnWhileStopped(v, { asked, downAt, target, answered: (x) => /\/stream/i.test(x.url) && x.status === 206 });
        const served = Number(flavour.elsewhere ? b : a);
        const after = seen.answers.filter((x) => x.at > seekAt && /\/videos\/[^/]+\/stream/i.test(x.url) && x.status === 206 && x.backend);
        if (!after.length || after.some((x) => Number(x.backend) !== served)) throw new Error(`the ranges after the seek should come from replica ${served}: ${after.map((x) => x.backend)}`);
        stopped = false;
        await endOutage(v, asked, downAt);
      });
    } finally {
      if (stopped) await postgres("start");
      await routerControl({ reset: true });
      await waitFor("every replica to be ready again", async () => {
        const state = await routerControl();
        return [...state.api, ...state.stream].every((x) => x.up);
      }, 60000);
    }
  });
}

// A worker killed while it serves a playing film. Two other workers run, and the
// main one is stopped for the scenario so that it takes no job: the film is
// served by one of the two, which is then killed with SIGKILL. The other takes
// the job over at the first missing segment, and the picture goes on: no dialog,
// no failed request, and no freeze near the ten seconds after which hls.js gives
// up on a request.
function parentOf(pid) {
  return Number(execFileSync("ps", ["-o", "ppid=", "-p", String(pid)]).toString().trim());
}

for (const flavour of [{ name: "re-encoded", film: "Failover Encode Film", copy: false }, { name: "copied", film: "Failover Copy Film", copy: true }]) await scenario("a worker killed mid-film is replaced by another and the film plays on" + `: ${flavour.name}`, async ({ page, seen }) => {
  const launch = process.env.E2E_WORKER;
  if (!launch) throw new Error("the failover scenarios need E2E_WORKER (test/e2e/run.sh sets it)");
  process.kill(workerPid, "SIGSTOP");
  const workers = [];
  try {
    for (let i = 0; i < 2; i++) workers.push(spawn(launch, [], { stdio: "ignore" }));
    // Rows of workers killed by the scenario before are forgotten at the next look for dead ones, so count only the rows whose lock is held.
    await waitFor("both workers to hold their locks", async () => sql(`SELECT count(*) FROM transcode_nodes n WHERE slots > 0 AND locked AND EXISTS (SELECT 1 FROM pg_locks l
      WHERE l.locktype = 'advisory' AND l.classid = 27219 AND l.objsubid = 2 AND l.objid::bigint = hashtext(n.id)::bigint & 4294967295)`) === "3", 60000);
    await play(page, "Transcode", flavour.film);
    await advances(page, 1);
    const runs = ffmpegRuns();
    if (runs.length !== 1 || runs[0].copy !== flavour.copy) throw new Error(`expected one ${flavour.name} ffmpeg, saw ${JSON.stringify(runs.map((r) => r.line.slice(0, 200)))}`);
    const owner = parentOf(runs[0].line.split(" ")[0]);
    const victim = workers.find((w) => w.pid === owner);
    const survivor = workers.find((w) => w !== victim);
    if (!victim) throw new Error(`ffmpeg belongs to process ${owner}, not one of the two workers`);
    const before = await time(page);
    const [victimJob, victimWorker] = sql("SELECT id || ' ' || worker FROM transcode_jobs WHERE state = 'running' ORDER BY id DESC LIMIT 1").split(" ");
    const killed = Date.now();
    victim.kill("SIGKILL");
    // currentTime must go on: the film reaches 30 s past the kill. How long it stood still is logged and not judged (a re-encode stood still 2.5 to 2.8 s, a copy 0.3 s, on a fast machine); what makes it right is below: the surviving worker made the later segments, no request failed or was abandoned, no dialog showed. The deadline only stops a hang.
    const survivorHasFfmpeg = () => ffmpegRuns().some((r) => parentOf(r.line.split(" ")[0]) === survivor.pid);
    const deadline = Date.now() + 300000;
    let last = before, moved = Date.now(), freeze = 0, taken = null;
    while (last < before + 30 && Date.now() < deadline) {
      if (await page.getByText("Playback Error").count()) throw new Error("the viewer got a Playback Error dialog");
      const now = await time(page);
      if (now > last + 0.05) {
        freeze = Math.max(freeze, Date.now() - moved);
        moved = Date.now();
        last = now;
      }
      if (!taken && survivorHasFfmpeg()) taken = Date.now() - killed;
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
    if (last < before + 30) throw new Error(`the film reached ${last.toFixed(1)} s, killed at ${before.toFixed(1)} s`);
    freeze = Math.max(freeze, Date.now() - moved);
    console.log(`      SIGKILL at ${before.toFixed(1)} s: the other worker's ffmpeg started ${taken} ms after it, the longest stand-still was ${freeze} ms`);
    if (taken === null) throw new Error("the other worker never started an ffmpeg");
    const replacements = sql(`SELECT count(*) FROM transcode_jobs WHERE id > ${victimJob} AND worker <> '${victimWorker}' AND next_segment > start_segment`);
    if (replacements === "0") throw new Error(`no job on a worker other than the killed one's (${victimWorker}) made segments after the kill`);
    // What happened, whatever the clock: the surviving worker made the job's later segments, no request failed or was abandoned, no error dialog showed.
    if (seen.failed.length) throw new Error(`requests ended without an answer: ${seen.failed.slice(0, 3).join("; ")}`);
    if (await page.getByText("Playback Error").count()) throw new Error("the viewer got a Playback Error dialog");
    noErrors(seen);
    await leave(page);
    await waitFor("ffmpeg to exit after Stopped", async () => ffmpegRuns().length === 0, 30000);
  } finally {
    for (const w of workers) w.kill("SIGKILL");
    process.kill(workerPid, "SIGCONT");
  }
}, { stopsWorker: true });

// Parallel transcoding (JS-024). Every worker's ffmpeg reads at 0.7 of the film's
// speed, so one worker alone cannot keep up: a viewer who watched it would stall
// for a third of the film. Two more workers run beside the main one, and helper
// jobs encode later chunks of the same film on them. hls.js plays through the
// seams between the chunks, seeks back and forth across them, and arrives at
// a point of the film sooner than one worker at that speed could take it.
await scenario("a slow re-encode is shared by three workers, and hls.js plays and seeks across the chunk boundaries", async ({ page, seen }) => {
  const launch = process.env.E2E_WORKER;
  if (!launch) throw new Error("the parallel scenario needs E2E_WORKER (test/e2e/run.sh sets it)");
  const rate = `${dirname(launch)}/rate`;
  writeFileSync(rate, "0.7");
  const workers = [];
  try {
    for (let i = 0; i < 2; i++) workers.push(spawn(launch, [], { stdio: "ignore" }));
    await waitFor("three workers to hold their locks", async () => sql(`SELECT count(*) FROM transcode_nodes n WHERE slots > 0 AND locked AND EXISTS (SELECT 1 FROM pg_locks l
      WHERE l.locktype = 'advisory' AND l.classid = 27219 AND l.objsubid = 2 AND l.objid::bigint = hashtext(n.id)::bigint & 4294967295)`) === "3", 60000);
    // Job rows outlive their scenario, and earlier scenarios left seek and replacement jobs past segment 0 on other workers: only the rows this play added count.
    const firstJob = Number(sql("SELECT COALESCE(max(id), 0) FROM transcode_jobs"));
    await play(page, "Transcode", "Parallel Encode Film");
    const began = Date.now();
    const helper = await waitFor("a helper job on another worker", async () => {
      const row = sql(`SELECT min(start_segment) FROM transcode_jobs WHERE id > ${firstJob} AND helper AND state = 'running'`);
      return row === "" ? null : Number(row);
    }, 60000);
    const boundary = helper * 4; // Seconds into the film where the main job's chunk ends.
    console.log(`      a helper started at segment ${helper}, ${Math.round((Date.now() - began) / 1000)} s into the film`);
    const workersBusy = sql("SELECT count(DISTINCT worker) FROM transcode_jobs WHERE state = 'running'");
    if (Number(workersBusy) < 2) throw new Error(`the helper runs on the main job's worker (${workersBusy} workers busy)`);

    // Plays through the first seam, and on to 60 s of the film. How long that took and how long the
    // picture stood still are logged and not judged: a slower runner takes longer on a right build.
    // What is judged is below, and the deadline only stops a hang.
    const target = 60;
    const deadline = Date.now() + 300000;
    let last = await time(page), moved = Date.now(), freeze = 0;
    while (last < target && Date.now() < deadline) {
      if (await page.getByText("Playback Error").count()) throw new Error("the viewer got a Playback Error dialog");
      const now = await time(page);
      if (now > last + 0.05) {
        freeze = Math.max(freeze, Date.now() - moved);
        moved = Date.now();
        last = now;
      }
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
    if (last < target) throw new Error(`the film reached ${last.toFixed(1)} s, the seam is at ${boundary} s`);
    console.log(`      reached ${target} s of the film ${Math.round((Date.now() - began) / 1000)} s after it started, the longest stand-still was ${freeze} ms`);
    // What happened, whatever the clock: a helper on another worker made segments past the seam, the player played them, no request failed or was abandoned.
    const made = sql(`SELECT string_agg(worker || ' ' || start_segment || ' ' || next_segment, ';') FROM transcode_jobs WHERE id > ${firstJob} AND helper AND next_segment > start_segment`).split(";").filter(Boolean).map((row) => row.split(" "));
    const mainWorker = sql(`SELECT worker FROM transcode_jobs WHERE id > ${firstJob} AND NOT helper AND start_segment = 0 ORDER BY id LIMIT 1`);
    const helped = made.filter(([worker, start]) => worker !== mainWorker && Number(start) >= helper);
    if (!helped.length) throw new Error(`no helper on another worker than the main job's (${mainWorker}) made segments: ${JSON.stringify(made)}`);
    const played = (n) => seen.hls.some((r) => r.status === 200 && Number(r.path.match(/\/(\d+)\.mp4$/i)?.[1]) === n);
    if (!helped.some(([, start, next]) => played(Number(start)) && Number(next) > Number(start))) throw new Error(`the player never fetched a segment a helper made: ${JSON.stringify(helped)}`);
    if (seen.failed.length) throw new Error(`requests ended without an answer: ${seen.failed.slice(0, 3).join("; ")}`);
    // The seam left no gap in what the player holds: one range covers it.
    const covered = await page.evaluate((at) => {
      const ranges = document.querySelector("video").buffered;
      for (let i = 0; i < ranges.length; i++) if (ranges.start(i) <= at - 1 && ranges.end(i) >= at + 1) return true;
      return false;
    }, boundary);
    if (!covered) throw new Error(`the buffer has a gap around ${boundary} s, the seam between two chunks`);

    // Seeks back across it, to just before it, and on past it, and then far.
    for (const to of [2, boundary - 1.5, boundary + 6, (await duration(page)) * 0.8]) {
      await page.evaluate((t) => { document.querySelector("video").currentTime = t; }, to);
      const seeked = Date.now();
      await waitFor(`playback past ${to.toFixed(1)} s after a seek`, async () => (await time(page)) > to + 2, 45000);
      if (await page.getByText("Playback Error").count()) throw new Error(`a Playback Error dialog after the seek to ${to.toFixed(1)} s`);
      console.log(`      seek to ${to.toFixed(1)} s played on after ${Date.now() - seeked} ms`);
    }
    noErrors(seen);
    await leave(page);
    await waitFor("every ffmpeg to exit after Stopped", async () => ffmpegRuns().length === 0, 30000);
    if (sql("SELECT count(*) FROM transcode_jobs WHERE state IN ('queued', 'running')") !== "0") throw new Error("a job is still live after Stopped");
  } finally {
    for (const w of workers) w.kill("SIGKILL");
    rmSync(rate, { force: true });
  }
});

// These stop the worker or need it gone, so they are marked and run last, on the
// last shard (test/e2e/shard.mjs). A viewer whose transcode can't start must
// be told, not left to jellyfin-web's retries, which never end on their own.
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
  // The player was answered, not left to abandon the request: a request it gave up on ends without an answer, and a seek aborts only the requests it replaces.
  if (answer.status !== 503 && answer.status !== 500) throw new Error(`the segment request was answered ${answer.status}`);
  if (seen.failed.some((f) => /ERR_TIMED_OUT|ERR_CONNECTION/.test(f))) throw new Error(`requests ended without an answer: ${seen.failed.join("; ")}`);
  const asked = [seen.hls.length, seen.infos.length];
  await new Promise((resolve) => setTimeout(resolve, 12000));
  if (seen.hls.length !== asked[0] || seen.infos.length !== asked[1]) throw new Error("the client kept asking after the refusal");
  // A killed worker's lock is gone, so it counts at once for nothing.
  if (!/a transcode job found (no worker|[^\n]*cores)/.test(readFileSync(process.env.E2E_LOG, "utf8"))) throw new Error("the log doesn't say why the job found no worker");
}, { stopsWorker: true });

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
}, { stopsWorker: true });

// Still without a worker: a film that direct plays keeps playing when the
// viewer picks its other audio track, which would have needed a transcode.
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
}, { stopsWorker: true });

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
