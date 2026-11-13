// Records the JSON responses a gate's browser receives from jellyscale, one line each, for
// compat/conform/cmd/conform to hold to Jellyfin 12.4's schema (JS-070). test/e2e/run.sh sets
// E2E_RESPONSES to the file, runs the checker over it when the gate ends, and fails the gate
// if it found a violation or no response.
import { appendFileSync } from "node:fs";

const file = process.env.E2E_RESPONSES;

// recordResponses(page, base): call once for each page a gate opens.
export function recordResponses(page, base) {
  if (!file) throw new Error("E2E_RESPONSES is the file for the recorded responses (test/e2e/run.sh sets it)");
  page.on("response", async (response) => {
    if (!response.url().startsWith(base)) return;
    const contentType = response.headers()["content-type"] ?? "";
    if (!/json/i.test(contentType)) return;
    const url = new URL(response.url());
    let body;
    try {
      body = await response.text();
    } catch {
      return; // the page went on before the body arrived; there is nothing to check
    }
    const method = response.request().method();
    appendFileSync(file, JSON.stringify({ request: `${method} ${url.pathname}`, method, path: url.pathname + url.search, status: response.status(), contentType, body }) + "\n");
  });
}
