// continuity() of test/e2e/recovery.mjs on positions a player could have: a fallback that restarts the film, one
// that skips ahead, one that goes on, each as the samples of a page. Run: node test/e2e/recovery_test.mjs
// (test/scripts_test.sh does).
import assert from "node:assert/strict";
import test from "node:test";
import { continuity } from "./recovery.mjs";

// Samples every 100 ms of wall clock, the position moving by `step` seconds each unless `at` says otherwise.
const play = (from, count, step = 0.1, w0 = 0) => Array.from({ length: count }, (_, i) => ({ t: from + i * step, w: w0 + i * 100 }));

test("playing on at the seek target is no problem", () => assert.deepEqual(continuity(play(61, 300), 61), []));
test("the seek's own jump before the target is not counted", () => {
  const jump = [{ t: 20, w: 0 }, { t: 20.1, w: 100 }, ...play(61, 100, 0.1, 200)];
  assert.deepEqual(continuity(jump, 61), []);
});
test("a restart at the film's start is found", () => {
  const restart = [...play(61, 50), ...play(0, 50, 0.1, 5000)];
  assert.match(continuity(restart, 61).join(), /went back to 0\.0 s from 65\.9 s/);
});
test("going back by one segment is allowed, by more is not", () => {
  assert.deepEqual(continuity([...play(70, 10), ...play(67, 10, 0.1, 1000)], 70), []);
  assert.equal(continuity([...play(70, 10), ...play(65.5, 10, 0.1, 1000)], 70).length > 0, true);
});
test("a jump forward of more than a second beyond the clock is found, one of a second is not", () => {
  assert.match(continuity([...play(61, 10), ...play(63, 10, 0.1, 1000)], 61).join(), /skipped forward/);
  assert.deepEqual(continuity([...play(61, 10), ...play(61.9 + 0.1, 10, 0.1, 1000)], 61), []);
});
test("a position that never reaches the target says so", () => assert.match(continuity(play(10, 20), 61).join(), /never reached 61 s/));
