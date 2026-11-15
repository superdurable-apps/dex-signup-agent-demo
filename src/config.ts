// Application configuration from environment variables: `.env` locally (`npm start` loads it), the Project
// configuration's application environment on Superverse. Secrets are never logged.

import type { ReviewChannel } from "./review-replies.ts";

export interface SignupAgentConfig {
  readonly reviewApiUrl: string;
  readonly reviewApiKey: string;
  /** The review channel and the reviewers: from the environment locally, from the reply binding when hosted. */
  readonly channel: ReviewChannel;
  readonly harness: "pi" | "claude-agent-sdk";
  readonly piModel: string;
  /** The model key pi uses; without it pi-ai reads ANTHROPIC_API_KEY from the process environment. */
  readonly anthropicApiKey: string | undefined;
  readonly claudeModel: string;
  /** `login` (the Claude Code login on this machine) or `api-key` (ANTHROPIC_API_KEY). */
  readonly claudeAuthentication: string;
  readonly claudeCliPath: string | undefined;
  readonly githubToken: string | undefined;
  readonly watchIntervalMs: number;
  /** Only sign-ups registered at or after this time are reviewed. */
  readonly reviewSince: Date;
  /** The local Worker's execution slots: a running turn or tool call takes one; a review that waits holds none. */
  readonly workerSlots: number;
  /** The local blob cache; a hosted process uses DEX_BLOB_CACHE_DIR/<mode>. */
  readonly blobCacheDirectory: string;
}

/** SIGNUP_REVIEW_SINCE is required, so a first start never reviews the whole backlog of pending sign-ups. */
function reviewSince(value: string): Date {
  const since = new Date(value);
  if (!/^\d{4}-\d{2}-\d{2}T/.test(value) || Number.isNaN(since.getTime())) {
    throw new Error(`SIGNUP_REVIEW_SINCE must be an RFC 3339 time, such as ${new Date().toISOString()}`);
  }
  return since;
}

function positiveInteger(value: string, name: string, unit = "seconds"): number {
  if (!/^[1-9]\d{0,5}$/.test(value)) {
    throw new Error(`${name} must be a positive whole number of ${unit}`);
  }
  return Number(value);
}

/**
 * Reads the configuration. A hosted process passes the review channel and reviewers of its Project's reply binding,
 * their only source there; locally they come from SLACK_REVIEW_CHANNEL and SIGNUP_REVIEWER_SLACK_IDS.
 */
export function loadSignupAgentConfig(environment: NodeJS.ProcessEnv = process.env, hostedChannel?: ReviewChannel): SignupAgentConfig {
  const optional = (name: string): string | undefined => environment[name]?.trim() || undefined;
  const required = (name: string): string => optional(name) ?? (() => { throw new Error(`${name} is required; see .env.example`); })();
  const harness = optional("SIGNUP_AGENT_HARNESS") ?? "pi";
  if (harness !== "pi" && harness !== "claude-agent-sdk") {
    throw new Error("SIGNUP_AGENT_HARNESS must be pi or claude-agent-sdk");
  }
  if (hostedChannel !== undefined && harness !== "pi") {
    throw new Error("SIGNUP_AGENT_HARNESS must be pi on Superverse: the Claude Agent SDK harness runs only locally");
  }
  return {
    reviewApiUrl: required("REVIEW_API_URL"),
    reviewSince: reviewSince(required("SIGNUP_REVIEW_SINCE")),
    reviewApiKey: required("REVIEW_API_KEY"),
    channel: hostedChannel ?? {
      channelId: required("SLACK_REVIEW_CHANNEL"),
      reviewerIds: new Set(required("SIGNUP_REVIEWER_SLACK_IDS").split(",").map((id) => id.trim()).filter(Boolean)),
    },
    harness,
    piModel: optional("SIGNUP_AGENT_PI_MODEL") ?? "anthropic/claude-opus-5-5",
    anthropicApiKey: optional("ANTHROPIC_API_KEY"),
    claudeModel: optional("SIGNUP_AGENT_CLAUDE_MODEL") ?? "claude-opus-5-5",
    claudeAuthentication: optional("SIGNUP_AGENT_CLAUDE_AUTH") ?? "login",
    claudeCliPath: optional("SIGNUP_AGENT_CLAUDE_CLI"),
    githubToken: optional("GITHUB_TOKEN"),
    watchIntervalMs: positiveInteger(optional("SIGNUP_WATCH_INTERVAL_SECONDS") ?? "30", "SIGNUP_WATCH_INTERVAL_SECONDS") * 1_000,
    workerSlots: positiveInteger(optional("SIGNUP_WORKER_SLOTS") ?? "64", "SIGNUP_WORKER_SLOTS", "slots"),
    blobCacheDirectory: optional("DEX_BLOB_CACHE_DIR") ?? ".dex-blob-cache",
  };
}
