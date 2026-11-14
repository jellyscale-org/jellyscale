// The player helpers the e2e gates share. Each takes the page first.
export const time = (page) => page.evaluate(() => document.querySelector("video")?.currentTime ?? -1);

export async function waitFor(what, test, timeout = 30000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    const value = await test();
    if (value) return value;
    await new Promise((resolve) => setTimeout(resolve, 200));
  }
  throw new Error(`timed out waiting for ${what}`);
}

// Returns once the media has played `seconds` on from where it settled. A player
// that is given a new source (a viewer changing subtitle track) empties the
// element and then jumps it to the resume position; that jump is not playback,
// and a seek made before the player's own start seek is overwritten by it (JS-099).
export async function advances(page, seconds = 1.5) {
  let from = null;
  await waitFor(`currentTime to play ${seconds} s on`, async () => {
    const { t, playing } = await page.evaluate(() => {
      const video = document.querySelector("video");
      if (!video) return { t: -1, playing: false };
      return { t: video.currentTime, playing: !video.seeking && video.readyState >= 3 && !video.paused };
    });
    if (!playing) return false;
    from ??= t;
    return t > from + seconds;
  }, 40000);
}

// Seeks, and returns once the picture is playing a little past the target, so a
// frame read from it is one the player has really shown.
export async function seek(page, seconds) {
  await page.evaluate((t) => { document.querySelector("video").currentTime = t; }, seconds);
  await waitFor(`playback past ${seconds}`, async () => page.evaluate((t) => {
    const video = document.querySelector("video");
    return !video.seeking && video.readyState >= 3 && video.currentTime > t + 0.5 && video.currentTime < t + 3;
  }, seconds), 60000);
}
