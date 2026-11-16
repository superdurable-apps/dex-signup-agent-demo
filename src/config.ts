// Application configuration from environment variables: `.env` locally (`npm start` loads it), the Project
// configuration's application environment on Superverse. Secrets are never logged.

import type { ReviewChannel } from "./review-replies.ts";

export interface SignupAgentConfig {
  readonly reviewApiUrl: string;
  readonly reviewApiKey: string;
  /** The review channel and the reviewers: from the environment locally, from the reply binding when hosted. */
  readonly channel: ReviewChannel;
  readonly harness: "pi" | "claude-agent-sdk";
  /** The pi harness's model; undefined with the Claude Agent SDK harness. */
  readonly piModel: PiModelSelection | undefined;
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

/** The provider prefix of SIGNUP_AGENT_PI_MODEL that selects an OpenAI-compatible endpoint outside pi-ai's catalog. */
export const openAICompatibleProvider = "openai-compatible";

/**
 * The pi model: `<provider>/<model ID>` of pi-ai's catalog with an optional key (without one pi-ai reads the
 * provider's key from the process environment), or an OpenAI-compatible Chat Completions endpoint with its key.
 */
export type PiModelSelection =
  | { readonly kind: "catalog"; readonly name: string; readonly apiKey: string | undefined }
  | { readonly kind: "openai-compatible"; readonly modelId: string; readonly baseUrl: string; readonly apiKey: string };

/** SIGNUP_AGENT_PI_BASE_URL: an http(s) URL with a host and no query or fragment, before `/chat/completions`. */
function openAICompatibleBaseUrl(value: string): string {
  const invalid = (reason: string) => new Error(`SIGNUP_AGENT_PI_BASE_URL ${reason}; give the base URL only, such as https://llm.example.com/v1`);
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    throw invalid("is not a URL");
  }
  if ((url.protocol !== "http:" && url.protocol !== "https:") || url.hostname === "") {
    throw invalid("must be an http(s) URL with a host");
  }
  if (url.search !== "" || url.hash !== "" || value.includes("?") || value.includes("#")) {
    throw invalid("must not have a query or a fragment");
  }
  if (/\/chat\/completions\/?$/.test(url.pathname)) {
    throw invalid("must not end in /chat/completions, which pi-ai appends");
  }
  return value;
}

/**
 * SIGNUP_AGENT_PI_MODEL `openai-compatible/<model ID>` selects SIGNUP_AGENT_PI_BASE_URL with SIGNUP_AGENT_PI_API_KEY;
 * an `anthropic/…` model requires ANTHROPIC_API_KEY; any other catalog model uses its provider's key from the environment.
 */
function piModelSelection(optional: (name: string) => string | undefined, required: (name: string) => string): PiModelSelection {
  const name = optional("SIGNUP_AGENT_PI_MODEL") ?? "anthropic/claude-opus-5-5";
  const separator = name.indexOf("/");
  if (separator <= 0 || separator === name.length - 1) {
    throw new Error("SIGNUP_AGENT_PI_MODEL must be <provider>/<model ID>, such as anthropic/claude-opus-5-5 or openai-compatible/qwen3.8-flash-next");
  }
  const provider = name.slice(0, separator);
  if (provider === openAICompatibleProvider) {
    const baseUrl = openAICompatibleBaseUrl(required("SIGNUP_AGENT_PI_BASE_URL"));
    return { kind: "openai-compatible", modelId: name.slice(separator + 1), baseUrl, apiKey: required("SIGNUP_AGENT_PI_API_KEY") };
  }
  if (optional("SIGNUP_AGENT_PI_BASE_URL") !== undefined) {
    throw new Error(`SIGNUP_AGENT_PI_BASE_URL is set, but SIGNUP_AGENT_PI_MODEL ${name} is not ${openAICompatibleProvider}/<model ID>`);
  }
  return { kind: "catalog", name, apiKey: provider === "anthropic" ? required("ANTHROPIC_API_KEY") : undefined };
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
    piModel: harness === "pi" ? piModelSelection(optional, required) : undefined,
    claudeModel: optional("SIGNUP_AGENT_CLAUDE_MODEL") ?? "claude-opus-5-5",
    claudeAuthentication: optional("SIGNUP_AGENT_CLAUDE_AUTH") ?? "login",
    claudeCliPath: optional("SIGNUP_AGENT_CLAUDE_CLI"),
    githubToken: optional("GITHUB_TOKEN"),
    watchIntervalMs: positiveInteger(optional("SIGNUP_WATCH_INTERVAL_SECONDS") ?? "30", "SIGNUP_WATCH_INTERVAL_SECONDS") * 1_000,
    workerSlots: positiveInteger(optional("SIGNUP_WORKER_SLOTS") ?? "64", "SIGNUP_WORKER_SLOTS", "slots"),
    blobCacheDirectory: optional("DEX_BLOB_CACHE_DIR") ?? ".dex-blob-cache",
  };
}
