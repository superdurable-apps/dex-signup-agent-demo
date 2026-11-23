// A stand-in for the usage operations Dex Cloud does not serve yet (docs/usage-api.md): get-usage and list-active,
// answered from a JSON file that you edit while it runs, so a local onboarding can be walked through any usage story.
// The tests use the same class in memory. Never point a hosted Project at it.
//
//   node tools/usage-standin.ts [--port 8872] [--file .local/usage-standin.json]
//
// The file holds { "accounts": { "<accountId>": { ...any AccountUsage members... } } }; each request reads it again.
// A member left out takes its empty value, and `asOf` is always now. Times may be relative: "-3d", "-6h", "now".

import { randomBytes } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import { dirname, resolve } from "node:path";

import type { AccountUsage } from "../src/usage-api.ts";

/** One account as the stand-in keeps it: any usage members, plus what list-active returns. */
export type StandInAccount = Partial<Omit<AccountUsage, "milestones" | "counts">> & {
  email?: string;
  displayName?: string | null;
  milestones?: Partial<AccountUsage["milestones"]>;
  counts?: Partial<AccountUsage["counts"]>;
};

const emptyMilestones: AccountUsage["milestones"] = {
  firstSignInAt: null,
  teamSpaceCreatedAt: null,
  projectCreatedAt: null,
  firstAgentMessageAt: null,
  repositoryLinkedAt: null,
  firstConnectionAt: null,
  firstPreviewReadyAt: null,
  firstLiveAt: null,
};
const emptyCounts: AccountUsage["counts"] = {
  teamSpaces: 0,
  projects: 0,
  liveProjects: 0,
  connections: 0,
  members: 0,
  agentMessages7d: 0,
  agentMessages28d: 0,
  previewStarts7d: 0,
  previewFailures7d: 0,
  publishes28d: 0,
  triggerEvents7d: 0,
};

/** "now", "-3d", "-6h", "-30m", or an RFC 3339 time, as an RFC 3339 time; anything else unchanged. */
export function resolveTime(value: unknown, now: number): unknown {
  if (value === "now") {
    return new Date(now).toISOString();
  }
  const relative = typeof value === "string" ? /^-(\d+(?:\.\d+)?)([dhm])$/.exec(value) : null;
  if (relative === null) {
    return value;
  }
  const unit = { d: 86_400_000, h: 3_600_000, m: 60_000 }[relative[2] as "d" | "h" | "m"];
  return new Date(now - Number(relative[1]) * unit).toISOString();
}

/** The usage the stand-in answers for one account. */
export function usageOf(accountId: string, stored: StandInAccount, now: number): AccountUsage {
  const time = (value: unknown) => (resolveTime(value, now) ?? null) as string | null;
  const milestones = Object.fromEntries(Object.entries({ ...emptyMilestones, ...stored.milestones }).map(([key, value]) => [key, time(value)])) as AccountUsage["milestones"];
  return {
    accountId,
    asOf: new Date(now).toISOString(),
    status: stored.status ?? "active",
    approvedAt: time(stored.approvedAt ?? null),
    lastSignInAt: time(stored.lastSignInAt ?? null),
    lastActivityAt: time(stored.lastActivityAt ?? null),
    activeDays7d: stored.activeDays7d ?? 0,
    activeDays28d: stored.activeDays28d ?? 0,
    milestones,
    counts: { ...emptyCounts, ...stored.counts },
    connectors: stored.connectors ?? [],
    projects: (stored.projects ?? []).map((project) => ({
      ...project,
      createdAt: time(project.createdAt) ?? new Date(now).toISOString(),
      lastActivityAt: time(project.lastActivityAt),
      lastPreviewFailure: project.lastPreviewFailure === null || project.lastPreviewFailure === undefined ? null : { ...project.lastPreviewFailure, at: time(project.lastPreviewFailure.at) ?? new Date(now).toISOString() },
    })),
  };
}

export interface StandInOptions {
  /** The accounts as they are now. */
  readonly read: () => Record<string, StandInAccount>;
  readonly addUnknown?: (accountId: string, account: StandInAccount) => void;
  readonly port?: number;
  readonly apiKey?: string;
  readonly now?: () => number;
}

/** The stand-in server. */
export class UsageStandIn {
  public readonly apiKey: string;
  private readonly server: Server;
  private readonly read: () => Record<string, StandInAccount>;
  private readonly now: () => number;
  /** Adds an account get-usage does not know yet, as just approved and unused; without it the answer is not_found. */
  private readonly addUnknown: ((accountId: string, account: StandInAccount) => void) | undefined;

  private constructor(server: Server, options: StandInOptions, apiKey: string) {
    this.server = server;
    this.read = options.read;
    this.apiKey = apiKey;
    this.now = options.now ?? Date.now;
    this.addUnknown = options.addUnknown;
  }

  public static async start(options: StandInOptions): Promise<UsageStandIn> {
    let standIn: UsageStandIn | undefined;
    const server = createServer((request, response) => void standIn!.answer(request, response));
    standIn = new UsageStandIn(server, options, options.apiKey ?? `srk_standin_${randomBytes(24).toString("base64url")}`);
    await new Promise<void>((done) => server.listen(options.port ?? 0, "127.0.0.1", done));
    return standIn;
  }

  public get baseUrl(): string {
    return `http://127.0.0.1:${(this.server.address() as AddressInfo).port}`;
  }

  public async stop(): Promise<void> {
    await new Promise<void>((done) => this.server.close(() => done()));
  }

  private async answer(request: IncomingMessage, response: ServerResponse): Promise<void> {
    const reply = (status: number, body: object) => response.writeHead(status, { "content-type": "application/json", "cache-control": "no-store" }).end(JSON.stringify(body));
    if (request.headers.authorization !== `Bearer ${this.apiKey}`) {
      return void reply(401, { error: { code: "unauthenticated", message: "unauthenticated" } });
    }
    const chunks: Buffer[] = [];
    for await (const chunk of request) {
      chunks.push(chunk as Buffer);
    }
    let body: Record<string, unknown>;
    try {
      body = JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}") as Record<string, unknown>;
    } catch {
      return void reply(400, { error: { code: "invalid_argument", message: "the body is not JSON" } });
    }
    const now = this.now();
    const accounts = this.read();
    if (request.method === "POST" && request.url === "/v1/account-review/get-usage") {
      const accountId = String(body.accountId ?? "");
      let stored = accounts[accountId];
      if (stored === undefined && this.addUnknown !== undefined && accountId !== "") {
        stored = { approvedAt: new Date(now).toISOString() };
        this.addUnknown(accountId, stored);
      }
      return stored === undefined ? void reply(404, { error: { code: "not_found", message: `no account ${accountId}` } }) : void reply(200, { usage: usageOf(accountId, stored, now) });
    }
    if (request.method === "POST" && request.url === "/v1/account-review/list-active") {
      const since = Date.parse(String(body.approvedSince ?? ""));
      const rows = Object.entries(accounts)
        .map(([accountId, stored]) => ({ accountId, stored, approvedAt: resolveTime(stored.approvedAt, now) as string | undefined }))
        .filter((row) => (row.stored.status ?? "active") === "active" && row.approvedAt !== undefined && row.approvedAt !== null && Date.parse(row.approvedAt) >= (Number.isNaN(since) ? 0 : since))
        .sort((left, right) => Date.parse(left.approvedAt!) - Date.parse(right.approvedAt!))
        .map((row) => ({ accountId: row.accountId, email: row.stored.email ?? `${row.accountId}@example.com`, displayName: row.stored.displayName ?? null, approvedAt: row.approvedAt }));
      return void reply(200, { accounts: rows, nextPageToken: "" });
    }
    reply(404, { error: { code: "not_found", message: "unknown operation" } });
  }
}

/** The command line: serve the file, creating it with one example account when it is missing. */
async function main(): Promise<void> {
  const argument = (name: string) => process.argv[process.argv.indexOf(name) + 1];
  const file = resolve(process.argv.includes("--file") ? argument("--file")! : ".local/usage-standin.json");
  if (!existsSync(file)) {
    mkdirSync(dirname(file), { recursive: true });
    const example: Record<string, StandInAccount> = {
      "example-account": { email: "ada@example.com", displayName: "Ada Lovelace", approvedAt: "-5d", lastSignInAt: "-4d", lastActivityAt: "-4d", activeDays7d: 1, activeDays28d: 1, milestones: { firstSignInAt: "-4d", teamSpaceCreatedAt: "-4d" }, counts: { teamSpaces: 1, members: 1 } },
    };
    writeFileSync(file, `${JSON.stringify({ accounts: example }, null, 2)}\n`);
  }
  const read = () => (JSON.parse(readFileSync(file, "utf8")) as { accounts?: Record<string, StandInAccount> }).accounts ?? {};
  const standIn = await UsageStandIn.start({
    read,
    // A newly approved account appears in the file at its first read, so you can edit its usage from there.
    addUnknown: (accountId, account) => {
      writeFileSync(file, `${JSON.stringify({ accounts: { ...read(), [accountId]: account } }, null, 2)}\n`);
      console.log(`Added ${accountId} to ${file}`);
    },
    port: Number(process.argv.includes("--port") ? argument("--port") : 8872),
    ...(process.env.ONBOARDING_USAGE_API_KEY ? { apiKey: process.env.ONBOARDING_USAGE_API_KEY } : {}),
  });
  console.log(`Usage stand-in on ${standIn.baseUrl}, reading ${file}`);
  console.log(`ONBOARDING_USAGE_API_URL=${standIn.baseUrl}`);
  if (!process.env.ONBOARDING_USAGE_API_KEY) {
    console.log(`ONBOARDING_USAGE_API_KEY=${standIn.apiKey}`);
  }
}

if (import.meta.url === `file://${process.argv[1]}`) {
  await main();
}
