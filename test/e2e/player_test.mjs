// The player helpers the e2e gates share (test/e2e/player.mjs: time, advances
// and seek, each taking the page first), played against a page whose <video>
// does what jellyfin-web's did in the run of JS-099 that failed with "Cannot
// read properties of null (reading 'seeking')": a viewer who switches from a
// burnt-in subtitle track to none is given a new source in the SAME element.
// The element is emptied (currentTime 0, nothing loaded), the player seeks it
// to the viewer's resume position, and then plays on from there. A seek that a
// test makes before that last step is overwritten by it, and at the film's end
// the player stops and takes the element out of the page.
//
// Time here is the number of reads: each page.evaluate is one tick, 0.2 s of
// media while the element plays, so the order of events never depends on how
// fast the machine runs the helpers (their 200 ms wait between polls only
// decides how long this test takes). Run: node test/e2e/player_test.mjs
// (test/scripts_test.sh does).
import assert from "node:assert/strict";
import test from "node:test";
import { advances, seek } from "./player.mjs";

const RESUME = 102.4;
const FILM_END = 105;

// reload: the element was given its new source just before the first read.
// Without it the element has been playing at RESUME for a while.
function fakePage({ reload, gone = false }) {
  let tick = reload ? -1 : 9;
  let present = !gone;
  let time = reload ? 0 : RESUME;
  let readyState = reload ? 0 : 4;
  let seeking = false;
  let paused = reload;
  let seekingSince = -1;
  const video = {
    get currentTime() { return time; },
    set currentTime(t) {
      if (reload && tick < 3) return; // the player's own seek at tick 3 lands over it
      time = t;
      seeking = true;
      readyState = 1;
      seekingSince = tick;
    },
    get seeking() { return seeking; },
    get readyState() { return readyState; },
    get paused() { return paused; },
    pause() { paused = true; },
    play() { paused = false; },
  };
  // What the page does to the element between one read and the next.
  const step = () => {
    tick++;
    if (reload && tick === 1) [seeking, time, readyState] = [true, RESUME, 1];
    if (reload && tick === 2) [seeking, readyState] = [false, 4];
    if (reload && tick === 3) [time, paused, seeking, readyState] = [RESUME, false, false, 4];
    else if (seeking && seekingSince >= 0 && tick > seekingSince) [seeking, readyState] = [false, 4];
    else if (!paused && !seeking && readyState >= 3 && tick > 3) time += 0.2;
    if (time >= FILM_END) present = false;
  };
  return {
    video,
    async evaluate(fn, arg) {
      step();
      globalThis.document = { querySelector: (selector) => (selector === "video" && present ? video : null) };
      return fn(arg);
    },
  };
}

test("a seek after the subtitle change is not overwritten by the player's own seek to the resume position", async () => {
  const page = fakePage({ reload: true });
  await advances(page, 2);
  await seek(page, 60);
  assert.ok(page.video.currentTime > 60.5 && page.video.currentTime < 63, `playing at ${page.video.currentTime}`);
});

test("advances waits for the media to play on from where the player put it, not for the player's jump", async () => {
  const page = fakePage({ reload: true });
  await advances(page, 1.5);
  assert.ok(page.video.currentTime >= RESUME + 1.5, `returned at ${page.video.currentTime}, before the media played 1.5 s from ${RESUME}`);
});

test("a seek on a player that is playing lands and is read back playing", async () => {
  const page = fakePage({ reload: false });
  await seek(page, 60);
  assert.ok(page.video.currentTime > 60.5 && page.video.currentTime < 63, `playing at ${page.video.currentTime}`);
});

test("a player that has left the film still fails the seek", async () => {
  await assert.rejects(seek(fakePage({ reload: false, gone: true }), 60));
});
