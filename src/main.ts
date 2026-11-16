// The entry of package.json's main. On Dex Cloud, `node . web` serves GET /api/health and the Slack events the
// platform relays to the reply Trigger on PORT, and `node . worker` runs the Dex Worker and the SignupWatcher with its
// readiness on the runtime's address. Locally, `node .` (npm start) runs all of it in one process, with the reply
// Trigger on Slack Socket Mode. Node.js runs as PID 1 on Live, which ignores SIGTERM without a handler, so each mode
// stops itself on SIGTERM and SIGINT.

import { createServer, type Server } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { Client, Registry, Worker, openBlobCache, type BlobCache } from "@superdurable/dex";

import { loadApplication, type Application } from "./application.ts";
import { dexOptions } from "./dex.ts";
import { createHttpServer } from "./http.ts";
import { isRelayedReplyRequest, serveRelayedReplies } from "./review-replies.ts";
import { loadRuntime, type RuntimeSettings } from "./runtime.ts";

const SERVER_STOP_GRACE_MS = 10_000;
const BLOB_CACHE_BYTES = 256 * 1024 * 1024;

const settings = loadRuntime(process.argv);
const application = await loadApplication({ mode: settings.mode });
if (settings.mode === "local") {
  await runLocal(application);
} else if (settings.mode === "web") {
  await runWeb(application);
} else {
  await runWorker(application, settings as RuntimeSettings);
}

/** A hosted process's blob cache is DEX_BLOB_CACHE_DIR/<mode>, under /tmp; the local one is the configured directory. */
function blobCache(mode: string): BlobCache {
  const directory = mode === "local" ? application.config.blobCacheDirectory : join(process.env.DEX_BLOB_CACHE_DIR || join(tmpdir(), "application-blobs"), mode);
  return openBlobCache({ directory, maxBytes: BLOB_CACHE_BYTES });
}

async function runLocal(application: Application): Promise<void> {
  const stop = stopSignal();
  const options = dexOptions(process.env);
  const registry = new Registry([...application.flows]);
  const cache = blobCache("local");
  const client = new Client(registry, cache, options);
  application.connect(client);
  // Locally nothing else publishes the Flow definitions, so the Worker uploads the session graph of its harness for
  // Dex Web's Design view; it skips the graph of the other harness, whose Flow type it does not register.
  const worker = new Worker(registry, cache, {
    ...options,
    pull: { maxConcurrentTasks: application.config.workerSlots },
    flowDefinitionPaths: [fileURLToPath(new URL("../flow-definitions", import.meta.url))],
  });
  await worker.start();
  await worker.waitPullRegistered({ timeoutMs: 30_000 });
  const running = application.start(client);
  running.done.catch((error: unknown) => fail("run the application", error));
  console.log(`Signup review agent (${application.sessionFlow.getFlowType()}) is running on ${options.serverAddress}`);
  await stop;
  console.log("Stopping; the reviews wait in Dex and go on when the agent starts again");
  await running.stop();
  await worker.close();
  await client.close();
  cache.close();
  process.exit(0);
}

async function runWeb(application: Application): Promise<void> {
  const stop = stopSignal();
  const registry = new Registry([...application.flows]);
  const cache = blobCache("web");
  const client = new Client(registry, cache, dexOptions(process.env));
  application.connect(client);
  let running: ReturnType<Application["start"]> | undefined;
  // A relayed event before the runner starts is answered 503, as the runner itself answers while it starts.
  const server = createHttpServer(application.routes(client), application.page, (incoming, outgoing) => {
    if (running?.replies !== undefined) {
      return serveRelayedReplies(running.replies, incoming, outgoing);
    }
    if (isRelayedReplyRequest(incoming)) {
      outgoing.writeHead(503).end();
      return true;
    }
    return false;
  });
  // The route listens before the runner registers it at the platform.
  await listen(server, Number(process.env.PORT || "8080"), "");
  running = application.start(client);
  running.done.catch((error: unknown) => fail("run the Slack reply Trigger", error));
  await stop;
  await close(server);
  await running.stop();
  await client.close();
  cache.close();
  process.exit(0);
}

async function runWorker(application: Application, settings: RuntimeSettings): Promise<void> {
  const stop = stopSignal();
  const options = dexOptions(process.env);
  const registry = new Registry([...application.flows]);
  const cache = blobCache("worker");
  // The platform publishes the Flow definitions, so the Worker uploads none.
  const worker = new Worker(registry, cache, {
    ...options,
    pull: { maxConcurrentTasks: settings.maxConcurrentTasks, stopDrainTimeoutMs: settings.stopDrainTimeoutMs, stopOnPermanentStreamRejection: true },
  });
  // The tools and the watcher read and start sessions through a Client, connected before the Worker pulls.
  const client = new Client(registry, cache, options);
  application.connect(client);
  let running: ReturnType<Application["start"]> | undefined;
  let ready = false;
  let stopping = false;
  // Readiness drops as soon as the stop begins; the drain follows.
  const readiness = createServer((incoming, outgoing) => {
    const isReady = ready && !stopping && worker.pullRegistrationState().isReady;
    outgoing.writeHead(incoming.url === "/readyz" && isReady ? 200 : 503);
    outgoing.end();
  });
  await listen(readiness, settings.readinessPort, settings.readinessHost);
  worker.run().then(
    () => stopping || fail("the Dex Worker stopped", undefined),
    (error: unknown) => stopping || fail("run the Dex Worker", error),
  );
  worker.waitPullRegistered({ timeoutMs: 60_000 }).then(
    () => {
      ready = true;
      running = application.start(client);
      running.done.catch((error: unknown) => stopping || fail("run the application", error));
    },
    (error: unknown) => stopping || fail("register the pull Worker", error),
  );
  await stop;
  stopping = true;
  await running?.stop();
  await worker.close();
  await client.close();
  cache.close();
  await close(readiness);
  process.exit(0);
}

function listen(server: Server, port: number, host: string): Promise<void> {
  return new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(port, host || undefined, () => resolve());
  });
}

/** Stops accepting connections and waits for the open requests, at most SERVER_STOP_GRACE_MS. */
function close(server: Server): Promise<void> {
  return new Promise((resolve) => {
    const grace = setTimeout(() => server.closeAllConnections(), SERVER_STOP_GRACE_MS);
    server.close(() => {
      clearTimeout(grace);
      resolve();
    });
    server.closeIdleConnections();
  });
}

function stopSignal(): Promise<void> {
  return new Promise((resolve) => {
    process.once("SIGTERM", () => resolve());
    process.once("SIGINT", () => resolve());
  });
}

function fail(what: string, error: unknown): never {
  console.error(JSON.stringify({ msg: "application stopped", error: `${what}: ${String(error)}` }));
  process.exit(1);
}
