// Forge entry point: bring the app server up, tell the trial
// environment where to find it, and stay alive until the environment
// says the trial is over.
//
// The sequence, in order:
//   * read MAKEFUN_ENV_URL, the trial environment's address. Without it
//     there is nobody to talk to, so quit straight away;
//   * start the app server on a port the kernel picks — never a fixed
//     one, because the environment itself owns 8080;
//   * POST the picked port to <env>/register so the environment can
//     dial back to this process;
//   * keep asking <env>/done until it answers that the trial finished;
//   * shut the server down and leave with status 0.
//
// The environment can be slow to wake, so every request is retried
// with a pause that grows between attempts: 250 ms at first, doubling
// each time, never longer than 2 s. A 200 or a 409 from /register both
// mean the port is recorded; a 400 or a 405 means the request itself
// is wrong and retrying it is pointless.

import { createApp } from "./app.ts";

// The longest a single HTTP round trip may take before it counts as
// a failed attempt and gets retried.
const REQUEST_BUDGET_MS = 30_000;

// The retry pause ladder: start here, double after each failure,
// never climb past the ceiling. The step cap exists so the ladder
// stops growing once it is already past the ceiling anyway.
const BACKOFF_START_MS = 250;
const BACKOFF_CEILING_MS = 2_000;
const BACKOFF_MAX_STEP = 8;

// The trial environment's address, with any trailing slashes removed.
const envBase = (process.env.MAKEFUN_ENV_URL ?? "").replace(/\/+$/, "");
if (envBase === "") {
  process.stderr.write("[forge] MAKEFUN_ENV_URL is missing, so there is no trial env to join\n");
  process.exit(1);
}

function pause(ms: number): Promise<void> {
  return new Promise((done) => setTimeout(done, ms));
}

function backoffMs(failures: number): number {
  const step = Math.min(failures, BACKOFF_MAX_STEP);
  return Math.min(BACKOFF_CEILING_MS, BACKOFF_START_MS * 2 ** step);
}

// What one probe attempt can report back to the retry loop.
type Verdict = "ok" | "fatal" | "again";

/**
 * Call `probe` over and over until it reports success. A probe that
 * throws counts as a failed attempt, not as the end of the world: the
 * environment may simply not be answering yet. Only a "fatal" verdict
 * stops the loop, because then another attempt could never succeed.
 */
async function retryUntilOk(label: string, probe: () => Promise<Verdict>): Promise<void> {
  let failures = 0;
  for (;;) {
    let verdict: Verdict;
    try {
      verdict = await probe();
    } catch {
      verdict = "again";
    }
    if (verdict === "ok") return;
    if (verdict === "fatal") throw new Error(`${label} was rejected by the trial env`);
    await pause(backoffMs(failures));
    failures += 1;
  }
}

// Tell the trial environment which port this process listens on.
async function announce(port: number): Promise<void> {
  await retryUntilOk("register", async () => {
    const res = await fetch(`${envBase}/register`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ port }),
      signal: AbortSignal.timeout(REQUEST_BUDGET_MS),
    });
    if (res.status === 200 || res.status === 409) return "ok";
    if (res.status === 400 || res.status === 405) return "fatal";
    return "again";
  });
}

// Ask the trial environment whether the trial is over yet. Never
// return until the answer is yes.
async function waitUntilDone(): Promise<void> {
  await retryUntilOk("done", async () => {
    const res = await fetch(`${envBase}/done`, {
      signal: AbortSignal.timeout(REQUEST_BUDGET_MS),
    });
    if (!res.ok) return "again";
    const payload = (await res.json()) as { done?: unknown };
    return payload.done === true ? "ok" : "again";
  });
}

// The whole life of the process: bind, announce, wait, shut down.
async function run(): Promise<void> {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, () => resolve());
  });
  const bound = server.address();
  if (bound === null || typeof bound === "string") {
    throw new Error("the server did not get a TCP port from the kernel");
  }
  process.stdout.write(`[forge] serving on port ${bound.port}\n`);

  await announce(bound.port);
  process.stdout.write("[forge] registered with the trial env\n");

  await waitUntilDone();
  server.close();
  process.stdout.write("[forge] trial finished, shutting down\n");
}

try {
  await run();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[forge] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
