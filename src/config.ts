// Application configuration from environment variables: `.env` locally (`npm start` loads it), the Project
// configuration's application environment on Superverse. Secrets are never logged.

import { getBuiltinModels, getBuiltinProviders, type BuiltinProvider } from "@earendil-works/pi-ai/providers/all";

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

/**
 * The provider of an OpenAI-compatible model outside pi-ai's catalog, and the legacy SIGNUP_AGENT_PI_MODEL prefix
 * `openai-compatible/<model ID>`, still accepted with SIGNUP_AGENT_PI_BASE_URL.
 */
export const openAICompatibleProvider = "openai-compatible";
/** The provider of an Anthropic-compatible model outside pi-ai's catalog, so pi-ai never applies its `anthropic` defaults. */
export const anthropicCompatibleProvider = "anthropic-compatible";

/** SIGNUP_AGENT_PI_PROTOCOL: the API of the endpoint at SIGNUP_AGENT_PI_BASE_URL. */
export const piEndpointProtocols = ["openai", "anthropic"] as const;
export type PiEndpointProtocol = (typeof piEndpointProtocols)[number];

/**
 * The pi model: `<provider>/<model ID>` of pi-ai's catalog with an optional key (without one pi-ai reads the
 * provider's key from the process environment), or a custom endpoint with its key: OpenAI-compatible Chat Completions
 * or Anthropic-compatible Messages.
 */
export type PiModelSelection =
  | { readonly kind: "catalog"; readonly name: string; readonly apiKey: string | undefined }
  | { readonly kind: "openai-compatible"; readonly modelId: string; readonly baseUrl: string; readonly apiKey: string }
  | { readonly kind: "anthropic-compatible"; readonly modelId: string; readonly baseUrl: string; readonly apiKey: string };

/** The path the client appends to the base URL of each protocol. */
const endpointPaths: Record<PiEndpointProtocol, { readonly path: string; readonly suffix: RegExp; readonly example: string }> = {
  openai: { path: "/chat/completions", suffix: /\/chat\/completions\/?$/, example: "https://llm.example.com/v1" },
  anthropic: { path: "/v1/messages", suffix: /\/messages\/?$/, example: "https://llm.example.com/anthropic" },
};

/** SIGNUP_AGENT_PI_BASE_URL: an http(s) URL with a host and no query or fragment, before the protocol's path. */
function endpointBaseUrl(value: string, protocol: PiEndpointProtocol): string {
  const { path, suffix, example } = endpointPaths[protocol];
  const invalid = (reason: string) => new Error(`SIGNUP_AGENT_PI_BASE_URL ${reason}; give the base URL only, such as ${example}`);
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
  if (suffix.test(url.pathname)) {
    throw invalid(`must not end in ${path.replace(/^\/v1/, "")}: with SIGNUP_AGENT_PI_PROTOCOL=${protocol} the client appends ${path}`);
  }
  return value;
}

/** The provider of a `<provider>/<model ID>` value when it is one of pi-ai's catalog, such as `anthropic`. */
function catalogProviderOf(name: string): BuiltinProvider | undefined {
  const provider = name.slice(0, Math.max(name.indexOf("/"), 0));
  return (getBuiltinProviders() as string[]).includes(provider) ? (provider as BuiltinProvider) : undefined;
}

/**
 * The pi model of SIGNUP_AGENT_PI_MODEL, SIGNUP_AGENT_PI_BASE_URL and SIGNUP_AGENT_PI_PROTOCOL:
 * - With SIGNUP_AGENT_PI_BASE_URL, the endpoint is custom, SIGNUP_AGENT_PI_API_KEY is required and
 *   SIGNUP_AGENT_PI_PROTOCOL picks its API, `openai` (the default) or `anthropic`. SIGNUP_AGENT_PI_MODEL is required.
 *   - `openai`: the endpoint's model ID as it is, slashes included (`qwen3.8-flash-next`, `meta-llama/Llama-3`), or
 *     the legacy `openai-compatible/<model ID>`. A value that starts with a pi-ai catalog provider, such as
 *     `anthropic/…`, is refused as ambiguous; an endpoint model ID that really starts with one is written
 *     `openai-compatible/<model ID>`.
 *   - `anthropic`: the endpoint's model ID as it is; the protocol was chosen explicitly, so no prefix is ambiguous.
 *     The legacy `openai-compatible/` prefix names the other protocol and is refused.
 * - Without it, SIGNUP_AGENT_PI_MODEL is `<provider>/<model ID>` of pi-ai's catalog, `anthropic/claude-opus-5-5` by
 *   default. An `anthropic/…` model requires ANTHROPIC_API_KEY; another catalog provider's key comes from its own
 *   variable. SIGNUP_AGENT_PI_PROTOCOL, `openai-compatible/…` and a value without a catalog provider, such as
 *   `openai` or `qwen3.8-flash-next`, are refused with a pointer to SIGNUP_AGENT_PI_BASE_URL.
 */
function piModelSelection(optional: (name: string) => string | undefined, required: (name: string) => string): PiModelSelection {
  const configured = optional("SIGNUP_AGENT_PI_MODEL");
  const configuredBaseUrl = optional("SIGNUP_AGENT_PI_BASE_URL");
  const configuredProtocol = optional("SIGNUP_AGENT_PI_PROTOCOL");
  const legacyPrefix = `${openAICompatibleProvider}/`;
  if (configuredProtocol !== undefined && !(piEndpointProtocols as readonly string[]).includes(configuredProtocol)) {
    throw new Error(`SIGNUP_AGENT_PI_PROTOCOL must be ${piEndpointProtocols.join(" or ")}, the API of the endpoint at SIGNUP_AGENT_PI_BASE_URL`);
  }
  if (configuredProtocol !== undefined && configuredBaseUrl === undefined) {
    throw new Error("SIGNUP_AGENT_PI_PROTOCOL applies only to a custom endpoint: set SIGNUP_AGENT_PI_BASE_URL too, or unset SIGNUP_AGENT_PI_PROTOCOL");
  }
  if (configuredBaseUrl !== undefined) {
    if (configured === undefined) {
      throw new Error("SIGNUP_AGENT_PI_MODEL is required with SIGNUP_AGENT_PI_BASE_URL: set it to the endpoint's model ID, such as qwen3.8-flash-next");
    }
    if ((configuredProtocol ?? "openai") === "anthropic") {
      if (configured.startsWith(legacyPrefix)) {
        throw new Error(`SIGNUP_AGENT_PI_MODEL ${configured} is the OpenAI-compatible form, but SIGNUP_AGENT_PI_PROTOCOL is anthropic: set SIGNUP_AGENT_PI_MODEL to the endpoint's model ID alone`);
      }
      return { kind: "anthropic-compatible", modelId: configured, baseUrl: endpointBaseUrl(configuredBaseUrl, "anthropic"), apiKey: required("SIGNUP_AGENT_PI_API_KEY") };
    }
    const modelId = configured.startsWith(legacyPrefix) ? configured.slice(legacyPrefix.length).trim() : configured;
    if (modelId === "") {
      throw new Error("SIGNUP_AGENT_PI_MODEL has no model ID: set it to the endpoint's model ID, such as qwen3.8-flash-next");
    }
    const provider = configured.startsWith(legacyPrefix) ? undefined : catalogProviderOf(configured);
    if (provider !== undefined) {
      throw new Error(
        `SIGNUP_AGENT_PI_MODEL ${configured} names the pi-ai catalog provider ${provider}, but SIGNUP_AGENT_PI_BASE_URL is set: ` +
          `unset SIGNUP_AGENT_PI_BASE_URL to use the catalog model, or write the endpoint's model ID as ${legacyPrefix}${configured}`,
      );
    }
    return { kind: "openai-compatible", modelId, baseUrl: endpointBaseUrl(configuredBaseUrl, "openai"), apiKey: required("SIGNUP_AGENT_PI_API_KEY") };
  }
  const name = configured ?? "anthropic/claude-opus-5-5";
  if (name.startsWith(legacyPrefix)) {
    throw new Error(`SIGNUP_AGENT_PI_MODEL ${name} needs SIGNUP_AGENT_PI_BASE_URL, the endpoint's base URL, such as https://llm.example.com/v1`);
  }
  const provider = catalogProviderOf(name);
  if (provider === undefined) {
    throw new Error(
      `SIGNUP_AGENT_PI_MODEL ${name} is not <provider>/<model ID> of pi-ai's catalog, such as anthropic/claude-opus-5-5. ` +
        "For an OpenAI-compatible endpoint, set SIGNUP_AGENT_PI_BASE_URL and SIGNUP_AGENT_PI_API_KEY, and SIGNUP_AGENT_PI_MODEL to the endpoint's model ID",
    );
  }
  const modelId = name.slice(provider.length + 1);
  if (!getBuiltinModels(provider).some((model) => model.id === modelId)) {
    throw new Error(`SIGNUP_AGENT_PI_MODEL ${name}: pi-ai's catalog has no model ${modelId} of ${provider}`);
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
