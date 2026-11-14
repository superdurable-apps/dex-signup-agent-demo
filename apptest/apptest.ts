// The API test harness of make test-app, from the Superverse node-ts base: every
// apptest runs the application's Flows on a pull Worker of a real Dex engine
// that exists only for that test, with the application's background work, and
// serves the real HTTP API in front of it. Never replace Dex; a test that
// cannot start its engine fails, it never skips.
import { spawn, type ChildProcess } from "node:child_process";
import { randomUUID } from "node:crypto";
import { createWriteStream, mkdirSync, type WriteStream, mkdtempSync, readFileSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { createServer as createNetServer } from "node:net";
import { finished } from "node:stream/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";

import { Client, Registry, Worker, openBlobCache, type FlowState, type FlowStatus } from "@superdurable/dex";

import { loadApplication, type ApplicationOptions } from "../src/application.ts";
import { createHttpServer } from "../src/http.ts";

/** How long a test waits for its engine, and at most for any condition. */
const ENGINE_WAIT_MS = 30_000;
const ENGINE_STOP_GRACE_MS = 10_000;

/** The application of one test: its HTTP API and the Dex Client behind it. */
export interface AppEnv {
  /** Calls the application's HTTP API, as the browser does, and decodes the JSON answer. */
  request(method: string, path: string, body?: unknown): Promise<{ status: number; body: any }>;
  /** For what Studio does, such as a Display read or a Stop; never use it to bypass the API. */
  readonly dex: Client;
  /** Polls condition every 100 ms until it holds; fails after timeoutMs (at most 30 s). Never sleep. */
  eventually(timeoutMs: number, condition: () => Promise<boolean>): Promise<void>;
}

/**
 * Declares the API test name: body runs against the application on its own
 * engine, which stops when the test ends. Name it TestSomething; the commit
 * gate's requirement trace and make test-app RUN='^TestSomething$' use it.
 * options are what src/application.ts loads the test's application with
 * besides its mode, such as a scripted model.
 */
export function apptest(name: string, body: (env: AppEnv) => Promise<void>, options: Omit<ApplicationOptions, "mode"> = {}): void {
  test(name, async () => {
    const cleanups: Cleanup[] = [];
    let failure: unknown;
    try {
      await body(await start(cleanups, name, options));
    } catch (error) {
      failure = error;
    }
    for (const cleanup of cleanups.reverse()) {
      try {
        await cleanup(failure !== undefined);
      } catch (error) {
        failure ??= error;
      }
    }
    recordOutcome(name, failure === undefined ? "passed" : "failed");
    if (failure !== undefined) {
      throw failure;
    }
  });
}

/** Ends what start opened, in reverse order; failed tells whether the test failed so far. */
type Cleanup = (failed: boolean) => Promise<void>;

async function start(cleanups: Cleanup[], name: string, options: Omit<ApplicationOptions, "mode">): Promise<AppEnv> {
  const application = await loadApplication({ ...options, mode: "test" });
  const engine = await startEngine(cleanups);
  const registry = new Registry([...application.flows]);
  const cacheDirectory = mkdtempSync(join(tmpdir(), "apptest-blobs-"));
  const cache = openBlobCache({ directory: cacheDirectory, maxBytes: 64 * 1024 * 1024 });
  cleanups.push(async () => {
    cache.close();
    rmSync(cacheDirectory, { recursive: true, force: true });
  });
  const worker = new Worker(registry, cache, {
    serverAddress: engine.address,
    attributeIndexSyncTimeoutMs: ENGINE_WAIT_MS,
    pull: { maxConcurrentTasks: 4, stopDrainTimeoutMs: 5_000 },
  });
  cleanups.push(() => worker.close());
  await worker.start();
  await worker.waitPullRegistered({ timeoutMs: ENGINE_WAIT_MS });
  const client = new Client(registry, cache, { serverAddress: engine.address });
  cleanups.push(() => client.close());
  // Runs before the client closes: the report reads the engine's executions.
  cleanups.push(() => recordEngine(name, client));
  // Adapted from the base: the application's background work (the watcher and the Slack reply Trigger) runs too, and
  // stops before the report reads the engine.
  const running = application.start(client);
  cleanups.push(() => running.stop());
  const server = createHttpServer(application.routes(client), application.page);
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  cleanups.push(() => new Promise<void>((resolve) => server.close(() => resolve())));
  const address = server.address();
  const base = `http://127.0.0.1:${typeof address === "object" && address !== null ? address.port : 0}`;
  return {
    dex: client,
    async request(method, path, body) {
      const response = await fetch(base + path, {
        method,
        headers: body === undefined ? undefined : { "content-type": "application/json" },
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(ENGINE_WAIT_MS),
      });
      const text = await response.text();
      return { status: response.status, body: text === "" ? undefined : JSON.parse(text) };
    },
    async eventually(timeoutMs, condition) {
      const limit = Math.min(timeoutMs, ENGINE_WAIT_MS);
      for (const deadline = Date.now() + limit; !(await condition()); ) {
        if (Date.now() > deadline) {
          throw new Error(`the condition did not hold within ${limit} ms`);
        }
        await new Promise((resolve) => setTimeout(resolve, 100));
      }
    },
  };
}

/**
 * Starts dex-server-zd dev, the binary make test-app names in
 * DEX_SERVER_ZD_BINARY, on a fresh loopback port with its data in the run
 * directory, and waits until it serves. A passing test's data is removed when
 * the engine stops; a failing one's stays with the failed run.
 */
async function startEngine(cleanups: Cleanup[]): Promise<{ address: string }> {
  const binary = process.env.DEX_SERVER_ZD_BINARY;
  if (!binary) {
    throw new Error("the test engine is not configured: run the tests with make test-app");
  }
  const engines = join(process.env.SUPERVERSE_APPTEST_RUN || tmpdir(), "engines");
  mkdirSync(engines, { recursive: true, mode: 0o700 });
  const directory = mkdtempSync(join(engines, "engine-"));
  let lastError: unknown;
  for (let attempt = 0; attempt < 3; attempt++) {
    const address = `127.0.0.1:${await freePort()}`;
    const log = createWriteStream(join(directory, "engine.log"), { flags: "a" });
    const engine = spawn(binary, ["dev", "--data-dir", join(directory, "data"), "--listen", address], { stdio: ["ignore", "pipe", "pipe"] });
    // Both streams write one log, which only stopEngine ends.
    engine.stdout.pipe(log, { end: false });
    engine.stderr.pipe(log, { end: false });
    try {
      await awaitServing(engine, address);
    } catch (error) {
      lastError = error;
      await stopEngine(engine, log);
      continue;
    }
    cleanups.push(async (failed) => {
      await stopEngine(engine, log);
      if (!failed) {
        rmSync(directory, { recursive: true, force: true });
      }
    });
    return { address };
  }
  const logged = readFileSync(join(directory, "engine.log"), "utf8");
  throw new Error(`the test engine did not serve: ${String(lastError)}: ${logged.slice(-2048)}`);
}

async function awaitServing(engine: ChildProcess, address: string): Promise<void> {
  const cacheDirectory = mkdtempSync(join(tmpdir(), "apptest-probe-"));
  const cache = openBlobCache({ directory: cacheDirectory, maxBytes: 1 << 20 });
  const probe = new Client(new Registry([]), cache, { serverAddress: address });
  try {
    for (const deadline = Date.now() + ENGINE_WAIT_MS; ; ) {
      if (engine.exitCode !== null || engine.signalCode !== null) {
        throw new Error(`the test engine exited (${engine.exitCode ?? engine.signalCode})`);
      }
      try {
        await probe.searchFlows("", 1);
        return;
      } catch (error) {
        if (Date.now() > deadline) {
          throw new Error(`the test engine did not serve within ${ENGINE_WAIT_MS} ms`, { cause: error });
        }
      }
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
  } finally {
    await probe.close();
    cache.close();
    rmSync(cacheDirectory, { recursive: true, force: true });
  }
}

/** Stops the engine (SIGTERM, SIGKILL after ENGINE_STOP_GRACE_MS), then ends its log once both streams ended. */
async function stopEngine(engine: ChildProcess, log: WriteStream): Promise<void> {
  let kill: NodeJS.Timeout | undefined;
  if (engine.exitCode === null && engine.signalCode === null) {
    kill = setTimeout(() => engine.kill("SIGKILL"), ENGINE_STOP_GRACE_MS);
    engine.kill("SIGTERM");
  }
  await Promise.all([finished(engine.stdout!), finished(engine.stderr!)]).catch(() => undefined);
  clearTimeout(kill);
  await new Promise<void>((resolve) => log.end(resolve));
}

/** A loopback port free now; the engine binds it next, and a competing bind fails that attempt. */
function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const server = createNetServer();
    server.once("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const address = server.address();
      server.close(() => resolve(typeof address === "object" && address !== null ? address.port : 0));
    });
  });
}

// The engine status names that make test-app's summary counts, as the Go harness writes them.
const STATUS_NAMES: Readonly<Record<FlowStatus, string>> = {
  running: "FLOW_STATUS_RUNNING",
  completed: "FLOW_STATUS_COMPLETED",
  failed: "FLOW_STATUS_FAILED",
  cancelled: "FLOW_STATUS_CANCELED",
  terminated: "FLOW_STATUS_TERMINATED",
  serverSideTimeoutInternalOnly: "FLOW_STATUS_SERVER_SIDE_TIMEOUT_INTERNAL_ONLY",
};

/** At most this many running executions of one test have their active Steps read; each recorded text is at most 1 KiB. */
const MAXIMUM_STATE_READS = 25;
const MAXIMUM_FAILURE_BYTES = 1024;

/** One execution of a test's engine report, in the Go harness's format. */
interface ReportedFlow {
  flowId: string;
  flowType: string;
  status: string;
  stateError?: string;
  failures?: Array<{ stepType: string; attempt: number; errorType?: string; detail?: string; stackTrace?: string }>;
}

/**
 * Writes what the test's engine held into report/engines/ for make test-app,
 * as the Go harness does: every execution with its status, the last failure
 * of each active Step of up to MAXIMUM_STATE_READS running ones, and how many
 * running ones it did not read. The commit gate checks the health of the
 * executions still running at a test's end from it.
 */
async function recordEngine(name: string, client: Client): Promise<void> {
  const directory = process.env.SUPERVERSE_APPTEST_REPORT_DIR;
  if (!directory) {
    return;
  }
  const report: { test: string; error?: string; executions: ReportedFlow[]; unread: number } = { test: name, executions: [], unread: 0 };
  let reads = 0;
  try {
    for (let token = ""; ; ) {
      const found = await client.searchFlows("", 100, token);
      for (const flow of found.flows) {
        const reported: ReportedFlow = { flowId: flow.flowId, flowType: flow.flowType, status: STATUS_NAMES[flow.status] };
        if (flow.status === "running") {
          if (reads === MAXIMUM_STATE_READS) {
            report.unread++;
          } else {
            reads++;
            await readFailures(client, reported);
          }
        }
        report.executions.push(reported);
      }
      if ((token = found.nextPageToken) === "") {
        break;
      }
    }
  } catch (error) {
    report.error = `search the executions: ${String(error)}`;
  }
  mkdirSync(join(directory, "engines"), { recursive: true, mode: 0o700 });
  writeFileSync(join(directory, "engines", `${randomUUID()}.json`), JSON.stringify(report), { mode: 0o600 });
}

/** Records the last failure of each active Step of the execution, or why its state was not read. */
async function readFailures(client: Client, flow: ReportedFlow): Promise<void> {
  let state: FlowState;
  try {
    state = await client.getFlowState(flow.flowId);
  } catch (error) {
    flow.stateError = truncate(String(error));
    return;
  }
  for (const step of state.activeStepExecutions) {
    const failure = step.lastFailure;
    if (failure === undefined) {
      continue;
    }
    // Since server-zd 0.1.25 backendError holds only the error category; the Worker's detail says more.
    const detail = failure.detail || failure.backendError;
    (flow.failures ??= []).push({
      stepType: step.stepType,
      attempt: failure.attempt,
      ...(failure.errorType ? { errorType: failure.errorType } : {}),
      ...(detail ? { detail: truncate(detail) } : {}),
      ...(failure.stackTrace ? { stackTrace: truncate(failure.stackTrace) } : {}),
    });
  }
}

/** The first MAXIMUM_FAILURE_BYTES bytes of text, cut at a character boundary. */
function truncate(text: string): string {
  const bytes = Buffer.from(text, "utf8");
  if (bytes.length <= MAXIMUM_FAILURE_BYTES) {
    return text;
  }
  // A character cut in two decodes to one trailing replacement character, which is dropped.
  return new TextDecoder().decode(bytes.subarray(0, MAXIMUM_FAILURE_BYTES)).replace(/�$/, "");
}

/**
 * Adds the test's outcome to tests.json in the run's report directory. make
 * test-app runs the test files one at a time, so the read and the write never
 * race.
 */
function recordOutcome(name: string, outcome: "passed" | "failed"): void {
  const directory = process.env.SUPERVERSE_APPTEST_REPORT_DIR;
  if (!directory) {
    return;
  }
  const path = join(directory, "tests.json");
  let tests: Array<{ name: string; outcome: string }> = [];
  try {
    tests = (JSON.parse(readFileSync(path, "utf8")) as { tests: typeof tests }).tests;
  } catch {
    tests = [];
  }
  tests.push({ name, outcome });
  writeFileSync(`${path}.new`, JSON.stringify({ tests }), { mode: 0o600 });
  renameSync(`${path}.new`, path);
}
