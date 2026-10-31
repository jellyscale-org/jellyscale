// Phase 3 gate: unmodified jellyfin-web shows text subtitles, from POSIX and S3,
// on direct play and on a transcode, and draws them into the picture when the
// viewer's settings ask for it. Needs the stack test/e2e/run.sh starts.
// Usage: E2E_SCRATCH=<worker scratch dir> E2E_PG=<postgres container> node subtitles.mjs <base_url> <user> <password>
// Run from a directory with playwright-core installed.
import { execFileSync } from "node:child_process";
import { createRequire } from "node:module";

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

const sql = (text) => execFileSync("docker", ["exec", process.env.E2E_PG, "psql", "-U", "postgres", "-Atc", text]).toString().trim();
const jobCount = () => Number(sql("SELECT count(*) FROM transcode_jobs"));

// The worker's ffmpeg processes, read from the process table.
function ffmpegRuns() {
  try {
    return execFileSync("pgrep", ["-af", `ffmpeg.*${scratch}`]).toString().trim().split("\n").filter(Boolean)
      .map((line) => ({ line, start: Number(/-start_number (\d+)/.exec(line)?.[1]), burns: /subtitles=subtitles\.ass/.test(line) }));
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
  const seen = { infos: [], playlists: [], subtitles: [], configs: 0, deletes: [], errors: [] };
  page.on("pageerror", (error) => seen.errors.push(error.message));
  page.on("request", (request) => {
    if (request.method() === "DELETE" && /ActiveEncodings/i.test(request.url())) seen.deletes.push(request.url());
  });
  page.on("response", async (response) => {
    if (!response.url().startsWith(base)) return;
    const url = new URL(response.url());
    if (response.status() >= 500) seen.errors.push(`${response.status()} ${url.pathname}`);
    if (/\.m3u8$/i.test(url.pathname)) seen.playlists.push(url.pathname + url.search);
    if (/\/Subtitles\//i.test(url.pathname)) {
      seen.subtitles.push({ status: response.status(), type: response.headers()["content-type"], path: url.pathname, index: Number(url.pathname.split("/")[5]) });
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

const time = (page) => page.evaluate(() => document.querySelector("video")?.currentTime ?? -1);

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
  const bad = seen.subtitles.filter((r) => r.status !== 200);
  if (bad.length) throw new Error(`subtitle requests failed: ${JSON.stringify(bad.slice(0, 3))}`);
}

async function scenario(name, run) {
  if (process.env.E2E_ONLY && !new RegExp(process.env.E2E_ONLY, "i").test(name)) return;
  const v = await viewer();
  try {
    await run(v);
    console.log(`ok    ${name}`);
  } catch (error) {
    const why = error.message.split("\n").filter(Boolean).slice(0, 3).join(" / ");
    failures.push(`${name}: ${why}`);
    console.log(`FAIL  ${name}: ${why}`);
    await v.page.screenshot({ path: `${process.env.TMPDIR ?? "/tmp"}/jellyscale-e2e-subtitles.png` }).catch(() => {});
  } finally {
    await v.context.close().catch(() => {});
  }
  await waitFor("ffmpeg to exit between scenarios", async () => ffmpegRuns().length === 0, 90000).catch(() => failures.push(`${name}: ffmpeg still running afterwards`));
}

const leave = async (page) => {
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

// Seeks, and returns once the picture is playing a little past the target, so a
// frame read from it is one the player has really shown.
async function seek(page, seconds) {
  await page.evaluate((t) => { document.querySelector("video").currentTime = t; }, seconds);
  await waitFor(`playback past ${seconds}`, async () => page.evaluate((t) => {
    const video = document.querySelector("video");
    return !video.seeking && video.readyState >= 3 && video.currentTime > t + 0.5 && video.currentTime < t + 3;
  }, seconds), 60000);
}

// How many bright pixels the picture has in its top, middle and bottom third,
// read from the paused frame. The burn-in film is black, so only text counts.
async function brightThirds(page) {
  await page.evaluate(() => document.querySelector("video").pause());
  await new Promise((resolve) => setTimeout(resolve, 400));
  const thirds = await page.evaluate(() => {
    const canvas = document.createElement("canvas");
    canvas.width = 320;
    canvas.height = 180;
    const context = canvas.getContext("2d", { willReadFrequently: true });
    context.drawImage(document.querySelector("video"), 0, 0, 320, 180);
    const data = context.getImageData(0, 0, 320, 180).data;
    const counts = [0, 0, 0];
    for (let i = 0; i < data.length; i += 4) {
      if (data[i] > 128 && data[i + 1] > 128 && data[i + 2] > 128) counts[Math.floor(i / 4 / 320 / 60)]++;
    }
    return counts;
  });
  await page.evaluate(() => document.querySelector("video").play());
  return thirds;
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
function clientRendered(name, library, title, transcoded) {
  return scenario(name, async ({ page, seen }) => {
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

await scenario("a film with only image subtitles plays as before and lists none", async ({ page, seen }) => {
  await play(page, "Transcode", "Upsilon Film");
  const streams = seen.infos.at(-1).source.MediaStreams;
  if (streams.some((s) => s.Type === "Subtitle")) throw new Error(`listed: ${JSON.stringify(streams.filter((s) => s.Type === "Subtitle"))}`);
  if (!(await page.evaluate(() => document.querySelector(".btnSubtitles").classList.contains("hide")))) throw new Error("the subtitle button is offered");
  await advances(page);
  if (seen.subtitles.length) throw new Error("subtitles were requested");
  noErrors(seen);
  await leave(page);
});

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

    // Choosing a track replaces the stream: PlaybackInfo, DELETE, a new master.
    const choose = async (index) => {
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
    };

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

await browser.close();
if (failures.length) {
  console.log("\nFAILED");
  for (const failure of failures) console.log(`  - ${failure}`);
  process.exit(1);
}
console.log("\nPASS");
