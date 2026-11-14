// What the scenarios that put Postgres away share (JS-103; transcode.mjs has its own copies of the first four, from JS-085).
// docker stop and start are what a restart does to the replicas: every connection ends and new ones are refused.
import { execFile } from "node:child_process";
import { spawn } from "node:child_process";
import { createServer } from "node:net";
import { promisify } from "node:util";
import { waitFor } from "./player.mjs";

const exec = promisify(execFile);

export async function postgres(action) {
  await exec("docker", [action, process.env.E2E_PG]);
  if (action === "start") {
    await waitFor("Postgres to answer again", async () => {
      try { await exec("docker", ["exec", process.env.E2E_PG, "psql", "-U", "postgres", "-h", "127.0.0.1", "-Atc", "select 1"]); return true; } catch { return false; }
    }, 60000);
  }
}

export const routerControl = async (base, body) => (await fetch(`${base}/__router`, body ? { method: "PUT", body: JSON.stringify(body) } : {})).json();

export const everyReplicaReady = async (base) => {
  const state = await routerControl(base);
  return [...state.api, ...state.stream].every((x) => x.up);
};

// Starts a second stream replica, B, and hands the body the ports of A (the one the stack started) and B. The router
// sends the stream role to A alone until the body says otherwise. The replica is stopped when the body is done, so a
// body that has Postgres away ends the outage first: stopping it earlier cuts the response of a range it is serving.
export async function withTwoStreamReplicas(base, body) {
  const port = await new Promise((resolve) => {
    const probe = createServer();
    probe.listen(0, "127.0.0.1", () => { const { port } = probe.address(); probe.close(() => resolve(port)); });
  });
  const child = spawn(process.env.E2E_STREAM_REPLICA, [String(port)], { stdio: "ignore" });
  try {
    await waitFor("the second stream replica to answer /healthz", async () => {
      try { return (await fetch(`http://127.0.0.1:${port}/healthz`)).ok; } catch { return false; }
    }, 60000);
    const [a] = (await routerControl(base)).stream.map((b) => b.port);
    return await body({ a, b: port });
  } finally {
    await routerControl(base, { reset: true });
    child.kill("SIGKILL");
  }
}
