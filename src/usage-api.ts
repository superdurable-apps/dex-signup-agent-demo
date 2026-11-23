// Client of the account usage operations the onboarding agent reads: one account's usage, and the active accounts
// approved since a time. They are a proposed extension of the Dex Cloud account review API (docs/usage-api.md); until
// the platform serves them, ONBOARDING_USAGE_API_URL points at the stand-in (scripts/usage-api-standin.ts).

import { z } from "zod";

const time = z.string().min(1);
const optionalTime = time.nullish().transform((value) => value ?? null);

/** When the account first reached each step of getting started; null until it does. */
const MilestonesSchema = z.object({
  firstSignInAt: optionalTime,
  teamSpaceCreatedAt: optionalTime,
  projectCreatedAt: optionalTime,
  firstAgentMessageAt: optionalTime,
  repositoryLinkedAt: optionalTime,
  firstConnectionAt: optionalTime,
  firstPreviewReadyAt: optionalTime,
  firstLiveAt: optionalTime,
});
export type Milestones = z.infer<typeof MilestonesSchema>;

const count = z.number().int().nonnegative();
const CountsSchema = z.object({
  teamSpaces: count,
  projects: count,
  liveProjects: count,
  connections: count,
  members: count,
  /** The account's messages to the Studio agent in the last 7 and 28 days. */
  agentMessages7d: count,
  agentMessages28d: count,
  previewStarts7d: count,
  previewFailures7d: count,
  publishes28d: count,
  /** Trigger events the account's Live applications received in the last 7 days. */
  triggerEvents7d: count,
});
export type UsageCounts = z.infer<typeof CountsSchema>;

const ProjectSchema = z.object({
  name: z.string().min(1).max(200),
  createdAt: time,
  lastActivityAt: optionalTime,
  stage: z.enum(["created", "building", "preview", "live"]),
  /** The last Preview start that failed, when the latest start failed. */
  lastPreviewFailure: z.object({ at: time, message: z.string().max(500) }).nullish().transform((value) => value ?? null),
});
export type ProjectUsage = z.infer<typeof ProjectSchema>;

const UsageSchema = z.object({
  accountId: z.string().min(1),
  /** When the server read the usage. */
  asOf: time,
  status: z.enum(["active", "disabled", "deleted"]),
  approvedAt: optionalTime,
  lastSignInAt: optionalTime,
  /** The latest sign-in, Studio message, Preview start or publish of the account. */
  lastActivityAt: optionalTime,
  /** Days with any such activity in the last 7 and 28 days. */
  activeDays7d: count,
  activeDays28d: count,
  milestones: MilestonesSchema,
  counts: CountsSchema,
  /** The connector IDs of the account's active connections, such as `slack`. */
  connectors: z.array(z.string().min(1).max(100)).max(100),
  /** The account's projects, the most recently active first, at most 20. */
  projects: z.array(ProjectSchema).max(20),
});
export type AccountUsage = z.infer<typeof UsageSchema>;

const ActiveAccountSchema = z.object({
  accountId: z.string().min(1),
  email: z.string().min(1),
  displayName: z.string().nullish(),
  approvedAt: time,
});
export type ActiveAccount = z.infer<typeof ActiveAccountSchema>;

/** A failed usage call; `not_found` for an account the server does not know. */
export class UsageApiError extends Error {
  public readonly code: string;
  public readonly httpStatus: number;

  public constructor(code: string, httpStatus: number, message: string) {
    super(message);
    this.name = "UsageApiError";
    this.code = code;
    this.httpStatus = httpStatus;
  }
}

/** Calls the usage operations: one POST with a JSON body each, under the review API's key scheme. Never logs the key. */
export class UsageApiClient {
  private readonly baseUrl: string;
  private readonly apiKey: string;

  public constructor(settings: { readonly baseUrl: string; readonly apiKey: string }) {
    const url = new URL(settings.baseUrl);
    if (url.protocol !== "https:" && url.hostname !== "127.0.0.1" && url.hostname !== "localhost") {
      throw new TypeError("ONBOARDING_USAGE_API_URL must use HTTPS");
    }
    this.baseUrl = settings.baseUrl.replace(/\/+$/, "");
    this.apiKey = settings.apiKey;
  }

  public async getUsage(accountId: string, signal?: AbortSignal): Promise<AccountUsage> {
    return UsageSchema.parse((await this.post("get-usage", { accountId }, signal)).usage);
  }

  /** Active accounts approved at or after `approvedSince`, oldest approval first; `pageSize` is at most 100. */
  public async listActiveAccounts(approvedSince: Date, pageSize: number, pageToken: string, signal?: AbortSignal) {
    const body = await this.post("list-active", { approvedSince: approvedSince.toISOString(), pageSize, pageToken }, signal);
    const accounts: ActiveAccount[] = [];
    for (const row of Array.isArray(body.accounts) ? (body.accounts as unknown[]) : []) {
      const parsed = ActiveAccountSchema.safeParse(row);
      if (parsed.success) {
        accounts.push(parsed.data);
      } else {
        console.warn("usage API list-active: skipped a malformed account");
      }
    }
    return { accounts, nextPageToken: String(body.nextPageToken ?? "") };
  }

  private async post(operation: string, payload: object, signal?: AbortSignal): Promise<Record<string, unknown>> {
    const timeout = AbortSignal.timeout(10_000);
    const response = await fetch(`${this.baseUrl}/v1/account-review/${operation}`, {
      method: "POST",
      headers: { "content-type": "application/json", authorization: `Bearer ${this.apiKey}` },
      body: JSON.stringify(payload),
      redirect: "error",
      signal: signal === undefined ? timeout : AbortSignal.any([signal, timeout]),
    });
    const body = (await response.json().catch(() => ({}))) as Record<string, unknown>;
    if (response.ok) {
      return body;
    }
    const error = (body.error ?? {}) as { code?: unknown; message?: unknown };
    const message = typeof error.message === "string" ? error.message.slice(0, 500) : `HTTP ${response.status}`;
    throw new UsageApiError(typeof error.code === "string" ? error.code : "unexpected", response.status, `usage API ${operation}: ${message}`);
  }
}
