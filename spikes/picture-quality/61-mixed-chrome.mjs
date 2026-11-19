// Does Chromium's MSE decode an H.264 fMP4 stream whose init segment came from one x264
// encode and whose later segments came from another? For each pair A:B, appends A/init,
// A/0, B/1..3; seeks inside B's part; compares frames (480x214) with the control B:B
// (B's own init + B/0..3); plays the stream muted at 4x and reads corrupted/dropped frames.
// Run: cd <playwright dir> && node <path>/61-mixed-chrome.mjs <mixed dir>
import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
// ESM ignores NODE_PATH, so resolve playwright-core from the working directory
const { chromium } = createRequire(path.join(process.cwd(), 'x.js'))('playwright-core');

const dir = path.resolve(process.argv[2]);
const pairs = ['veryfast:veryfast', 'veryfast:medium', 'veryfast:fast', 'medium:veryfast',
  'veryfast.ref3:fast.ref3', 'veryfast.ref3:slow.ref3', 'fast.ref3:slow.ref3',
  'veryfast.inband:medium.inband', 'medium.inband:veryfast.inband'];
const times = [5.0, 6.5, 9.0, 13.5];

const server = http.createServer((req, res) => {
  const f = path.join(dir, decodeURIComponent(req.url.split('?')[0]));
  if (!f.startsWith(dir) || !fs.existsSync(f)) { res.writeHead(404); return res.end(); }
  res.end(fs.readFileSync(f));
}).listen(0);
await new Promise(r => server.once('listening', r));
const base = `http://127.0.0.1:${server.address().port}`;

// codec string from the init's avcC box: profile, constraint flags, level
function codecOf(run) {
  const b = fs.readFileSync(path.join(dir, run, 'init.mp4'));
  const i = b.indexOf('avcC');
  return 'avc1.' + [b[i + 5], b[i + 6], b[i + 7]].map(x => x.toString(16).padStart(2, '0')).join('');
}

// Runs in the page: build a MediaSource from urls, seek to each time, grab frames, play through.
function inPage({ urls, codec, times, base }) {
  const sleep = ms => new Promise(r => setTimeout(r, ms));
  return (async () => {
    const out = { appended: true, errors: [], frames: [], quality: null, videoError: null };
    const video = document.createElement('video');
    video.muted = true;
    document.body.append(video);
    const ms = new MediaSource();
    video.src = URL.createObjectURL(ms);
    await new Promise(r => ms.addEventListener('sourceopen', r, { once: true }));
    const sb = ms.addSourceBuffer(`video/mp4; codecs="${codec}"`);
    sb.addEventListener('error', () => out.errors.push('sourcebuffer error event'));
    try {
      for (const u of urls) {
        const data = await (await fetch(base + u)).arrayBuffer();
        sb.appendBuffer(data);
        await new Promise((res, rej) => {
          sb.addEventListener('updateend', res, { once: true });
          sb.addEventListener('error', () => rej(new Error('append failed: ' + u)), { once: true });
        });
      }
      ms.endOfStream();
    } catch (e) { out.appended = false; out.errors.push(String(e)); }
    video.addEventListener('error', () => {
      out.videoError = video.error && `${video.error.code} ${video.error.message}`;
    });
    const canvas = document.createElement('canvas');
    canvas.width = 480; canvas.height = 214;
    const cx = canvas.getContext('2d', { willReadFrequently: true });
    for (const t of times) {
      if (video.error) { out.frames.push(null); continue; }
      const seeked = new Promise(r => video.addEventListener('seeked', r, { once: true }));
      video.currentTime = t;
      await Promise.race([seeked, sleep(20000)]);
      await Promise.race([new Promise(r => video.requestVideoFrameCallback(r)), sleep(1500)]);
      await sleep(200);
      cx.drawImage(video, 0, 0, 480, 214);
      out.frames.push(Array.from(cx.getImageData(0, 0, 480, 214).data));
    }
    if (!video.error) {
      video.currentTime = 0; video.playbackRate = 4;
      await Promise.race([video.play().catch(() => {}), sleep(5000)]);
      await Promise.race([new Promise(r => video.addEventListener('ended', r, { once: true })), sleep(30000)]);
    }
    const q = video.getVideoPlaybackQuality();
    out.quality = { corrupted: q.corruptedVideoFrames, dropped: q.droppedVideoFrames, total: q.totalVideoFrames };
    out.videoError = out.videoError || (video.error && `${video.error.code} ${video.error.message}`);
    video.remove();
    return out;
  })();
}

const browser = await chromium.launch({ headless: true, timeout: 120000 });
const page = await browser.newPage();
page.setDefaultTimeout(180000);
await page.goto(base + '/missing');
const supported = await page.evaluate(() => MediaSource.isTypeSupported('video/mp4; codecs="avc1.640033"'));
console.log(`browser: Chromium ${browser.version()} (playwright-core headless), H.264 MSE supported: ${supported}`);

const controls = new Map();
const control = async b => {
  if (!controls.has(b)) controls.set(b, await page.evaluate(inPage,
    { urls: [`/${b}/init.mp4`, ...[0, 1, 2, 3].map(i => `/${b}/${i}.m4s`)], codec: codecOf(b), times, base }));
  return controls.get(b);
};
const mad = (x, y) => { let s = 0; for (let i = 0; i < x.length; i++) s += Math.abs(x[i] - y[i]); return s / x.length; };

console.log('pair | appended | video.error | max mean-abs-diff (per time) | corrupted/dropped/total | verdict');
for (const pair of pairs) {
  const [a, b] = pair.split(':');
  const ctl = await control(b);
  const r = await page.evaluate(inPage, { urls: [`/${a}/init.mp4`, `/${a}/0.m4s`, `/${b}/1.m4s`, `/${b}/2.m4s`, `/${b}/3.m4s`], codec: codecOf(a), times, base });
  const diffs = r.frames.map((f, i) => f && ctl.frames[i] ? mad(f, ctl.frames[i]) : null);
  const max = diffs.every(d => d !== null) ? Math.max(...diffs) : null;
  const verdict = !r.appended || r.videoError || max === null ? 'error' : max > 5 ? 'corrupt' : 'decodes';
  const q = r.quality;
  console.log([pair, r.appended ? 'ok' : 'error: ' + r.errors.join(';'), r.videoError || 'none',
    max === null ? 'n/a' : `${max.toFixed(2)} (${diffs.map(d => d === null ? 'n/a' : d.toFixed(2)).join(', ')})`,
    `${q.corrupted}/${q.dropped}/${q.total}` + (ctl.videoError ? ` ctl-err=${ctl.videoError}` : ` ctl:${ctl.quality.corrupted}/${ctl.quality.dropped}/${ctl.quality.total}`),
    verdict].join(' | '));
}
await browser.close();
server.close();
