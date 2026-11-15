// The run mode and the platform's runtime.yaml, as the Superverse node-ts base reads them. `node . web` and
// `node . worker` are the two hosted processes; `node .` without a mode is the local process, which runs both roles.

import { readFileSync } from "node:fs";

import { parse } from "yaml";

/** The hosted HTTP process, the hosted Dex Worker process, or the local process with both roles. */
export type RunMode = "web" | "worker" | "local";

/** The platform-owned runtime.yaml that both hosted modes read, as Preview and Live write it. */
export interface RuntimeSettings {
  readonly mode: RunMode;
  /** Where the Worker answers GET /readyz: host (empty for every interface) and port. */
  readonly readinessHost: string;
  readonly readinessPort: number;
  /** The Worker slots of dex-app.yaml's application.workerSlots, 1 to 64. */
  readonly maxConcurrentTasks: number;
  /** How long a stopping Worker drains its running tasks. */
  readonly stopDrainTimeoutMs: number;
}

const RUNTIME_FILE = "/tmp/superverse-app/runtime.yaml";
const WORKER_KEYS = ["mode", "readinessAddress", "maxConcurrentTasks", "idleStreamsPerQueue", "stopDrainTimeout"];
const MAXIMUM_DRAIN_MS = 10 * 60 * 1000;

/**
 * Reads the mode from the first argument and, for a hosted mode, the platform's runtime.yaml, refusing anything the
 * platform does not write: an unknown key, a Worker that does not pull, or a value out of its range. Without an
 * argument the mode is local, which reads no runtime.yaml.
 */
export function loadRuntime(argv: readonly string[]): RuntimeSettings | { readonly mode: "local" } {
  if (argv.length === 2) {
    return { mode: "local" };
  }
  const mode = argv[2];
  if (argv.length !== 3 || (mode !== "web" && mode !== "worker")) {
    throw new Error("run the application with web or worker, or with no argument locally");
  }
  let document: unknown;
  try {
    document = parse(readFileSync(RUNTIME_FILE, "utf8"));
  } catch (error) {
    throw new Error(`read runtime.yaml: ${String(error)}`, { cause: error });
  }
  const root = asRecord(document, "runtime.yaml");
  if (Object.keys(root).some((key) => key !== "worker")) {
    throw new Error("runtime.yaml holds only worker");
  }
  const worker = asRecord(root.worker ?? {}, "runtime.yaml worker");
  const unknown = Object.keys(worker).filter((key) => !WORKER_KEYS.includes(key));
  if (unknown.length > 0) {
    throw new Error(`runtime.yaml worker has unknown keys: ${unknown.join(", ")}`);
  }
  const readiness = typeof worker.readinessAddress === "string" ? worker.readinessAddress : "";
  const separator = readiness.lastIndexOf(":");
  const readinessPort = Number(readiness.slice(separator + 1));
  const slots = worker.maxConcurrentTasks;
  const idle = worker.idleStreamsPerQueue;
  const drain = typeof worker.stopDrainTimeout === "string" ? parseDuration(worker.stopDrainTimeout) : undefined;
  if (
    separator < 0 ||
    !Number.isInteger(readinessPort) ||
    readinessPort < 1 ||
    readinessPort > 65535 ||
    drain === undefined ||
    drain <= 0 ||
    drain > MAXIMUM_DRAIN_MS ||
    !Number.isInteger(slots) ||
    (slots as number) < 1 ||
    (slots as number) > 64 ||
    !Number.isInteger(idle) ||
    (idle as number) < 1 ||
    (idle as number) > 8
  ) {
    throw new Error("runtime.yaml requires Worker readiness, a drain timeout within ten minutes, 1 to 64 Worker slots and 1 to 8 idle streams");
  }
  if (worker.mode !== "pull") {
    throw new Error("runtime.yaml worker.mode must be pull");
  }
  return { mode, readinessHost: readiness.slice(0, separator), readinessPort, maxConcurrentTasks: slots as number, stopDrainTimeoutMs: drain };
}

function asRecord(value: unknown, name: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`${name} must be a mapping`);
  }
  return value as Record<string, unknown>;
}

const DURATION_UNITS: Readonly<Record<string, number>> = { h: 3_600_000, m: 60_000, s: 1_000, ms: 1 };

/** Parses a Go duration of whole hours, minutes, seconds and milliseconds, such as 10s or 1m30s. */
function parseDuration(text: string): number | undefined {
  const parts = /^(?:(\d+)h)?(?:(\d+)m(?!s))?(?:(\d+)s)?(?:(\d+)ms)?$/.exec(text);
  if (parts === null || text === "") {
    return undefined;
  }
  return ["h", "m", "s", "ms"].reduce((total, unit, index) => total + Number(parts[index + 1] ?? 0) * DURATION_UNITS[unit]!, 0);
}
