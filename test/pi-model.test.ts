// The pi model selection of the configuration, and the OpenAI-compatible model against a local streaming endpoint.

import assert from "node:assert/strict";
import { createServer, type IncomingHttpHeaders } from "node:http";
import type { AddressInfo } from "node:net";
import test from "node:test";

import type { Context } from "@earendil-works/pi-ai";
import { streamSimple } from "@earendil-works/pi-ai/compat";

import { loadSignupAgentConfig } from "../src/config.ts";
import { configuredPiModel, openAICompatibleMaxTokens, openAICompatibleModel } from "../src/pi-agent.ts";

const baseEnvironment = {
  REVIEW_API_URL: "https://review-api.example.invalid",
  REVIEW_API_KEY: "srk_test_secret",
  SIGNUP_REVIEW_SINCE: "2026-11-13T00:00:00Z",
  SLACK_REVIEW_CHANNEL: "C0123456789",
  SIGNUP_REVIEWER_SLACK_IDS: "U01",
};
const configOf = (environment: Record<string, string>) => loadSignupAgentConfig({ ...baseEnvironment, ...environment });

test("the default pi model is Anthropic's and requires ANTHROPIC_API_KEY", () => {
  assert.deepEqual(configOf({ ANTHROPIC_API_KEY: "sk-ant" }).piModel, { kind: "catalog", name: "anthropic/claude-opus-5-5", apiKey: "sk-ant" });
  assert.throws(() => configOf({}), /ANTHROPIC_API_KEY is required/);
  assert.throws(() => configOf({ SIGNUP_AGENT_PI_MODEL: "anthropic/claude-opus-5-5", ANTHROPIC_API_KEY: " " }), /ANTHROPIC_API_KEY is required/);
});

test("another catalog model needs no ANTHROPIC_API_KEY: pi-ai reads its provider's key from the environment", () => {
  assert.deepEqual(configOf({ SIGNUP_AGENT_PI_MODEL: "openai/gpt-5" }).piModel, { kind: "catalog", name: "openai/gpt-5", apiKey: undefined });
});

test("openai-compatible/<model ID> selects the endpoint and its key, and needs no ANTHROPIC_API_KEY", () => {
  const environment = { SIGNUP_AGENT_PI_MODEL: "openai-compatible/qwen3.8-flash-next", SIGNUP_AGENT_PI_BASE_URL: "https://llm.example.com/v1", SIGNUP_AGENT_PI_API_KEY: "sk-qwen" };
  assert.deepEqual(configOf(environment).piModel, { kind: "openai-compatible", modelId: "qwen3.8-flash-next", baseUrl: "https://llm.example.com/v1", apiKey: "sk-qwen" });
  // A model ID may itself contain a slash.
  assert.deepEqual(configOf({ ...environment, SIGNUP_AGENT_PI_MODEL: "openai-compatible/Qwen/Qwen3-8B" }).piModel, { kind: "openai-compatible", modelId: "Qwen/Qwen3-8B", baseUrl: "https://llm.example.com/v1", apiKey: "sk-qwen" });
  assert.throws(() => configOf({ ...environment, SIGNUP_AGENT_PI_API_KEY: "" }), /SIGNUP_AGENT_PI_API_KEY is required/);
  assert.throws(() => configOf({ ...environment, SIGNUP_AGENT_PI_BASE_URL: "" }), /SIGNUP_AGENT_PI_BASE_URL is required/);
  assert.throws(() => configOf({ ...environment, SIGNUP_AGENT_PI_BASE_URL: "llm.example.com/v1" }), /SIGNUP_AGENT_PI_BASE_URL must be an http\(s\) URL/);
});

test("a base URL without the openai-compatible model, or a model without a provider, is refused", () => {
  assert.throws(() => configOf({ ANTHROPIC_API_KEY: "sk-ant", SIGNUP_AGENT_PI_BASE_URL: "https://llm.example.com/v1" }), /SIGNUP_AGENT_PI_BASE_URL is set/);
  assert.throws(() => configOf({ SIGNUP_AGENT_PI_MODEL: "qwen3.8-flash-next" }), /must be <provider>\/<model ID>/);
  assert.throws(() => configOf({ SIGNUP_AGENT_PI_MODEL: "openai-compatible/" }), /must be <provider>\/<model ID>/);
});

test("the Claude Agent SDK harness has no pi model and requires no model key", () => {
  const config = configOf({ SIGNUP_AGENT_HARNESS: "claude-agent-sdk" });
  assert.equal(config.piModel, undefined);
});

test("configuredPiModel builds a client for a catalog model and for an OpenAI-compatible one", () => {
  assert.ok(configuredPiModel({ kind: "catalog", name: "anthropic/claude-opus-5-5", apiKey: "sk-ant" }));
  assert.ok(configuredPiModel({ kind: "openai-compatible", modelId: "qwen3.8-flash-next", baseUrl: "https://llm.example.com/v1", apiKey: "sk-qwen" }));
  assert.throws(() => configuredPiModel({ kind: "catalog", name: "anthropic/no-such-model", apiKey: "sk-ant" }), /not in pi-ai's catalog/);
});

interface CapturedRequest {
  readonly url: string | undefined;
  readonly headers: IncomingHttpHeaders;
  readonly body: Record<string, any>;
}

/** A Chat Completions endpoint that streams the chunks once per request and records each request. */
async function streamingEndpoint(chunks: readonly object[]) {
  const requests: CapturedRequest[] = [];
  const server = createServer((request, response) => {
    let body = "";
    request.on("data", (piece) => (body += piece));
    request.on("end", () => {
      requests.push({ url: request.url, headers: request.headers, body: JSON.parse(body) });
      response.writeHead(200, { "content-type": "text/event-stream" });
      for (const chunk of chunks) {
        response.write(`data: ${JSON.stringify(chunk)}\n\n`);
      }
      response.end("data: [DONE]\n\n");
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  return { baseUrl: `http://127.0.0.1:${(server.address() as AddressInfo).port}/v1`, requests, close: () => new Promise((resolve) => server.close(resolve)) };
}

const chunk = (delta: object, finishReason: string | null = null) => ({
  id: "chatcmpl-1",
  object: "chat.completion.chunk",
  model: "qwen3.8-flash-next",
  choices: [{ index: 0, delta, finish_reason: finishReason }],
});

/** Tool-call deltas as the endpoint sends them: `"type":"function"` on every delta, the arguments split across them. */
const toolCallChunks = (repeatIdAndName: boolean) => [
  chunk({ role: "assistant", reasoning_content: "Check the " }),
  chunk({ reasoning_content: "profile first." }),
  chunk({ tool_calls: [{ index: 0, id: "call_1", type: "function", function: { name: "lookup_profile", arguments: "" } }] }),
  ...['{"account', 'Id":"a-1"', "}"].map((piece) =>
    chunk({ tool_calls: [{ index: 0, type: "function", ...(repeatIdAndName ? { id: "call_1" } : {}), function: { ...(repeatIdAndName ? { name: "lookup_profile" } : {}), arguments: piece } }] }),
  ),
  chunk({}, "tool_calls"),
  { id: "chatcmpl-1", object: "chat.completion.chunk", model: "qwen3.8-flash-next", choices: [], usage: { prompt_tokens: 40, completion_tokens: 12, total_tokens: 52 } },
];

const context: Context = {
  systemPrompt: "You review sign-ups.",
  messages: [{ role: "user", content: "Review account a-1.", timestamp: Date.now() }],
  tools: [{ name: "lookup_profile", description: "Looks up a profile.", parameters: { type: "object", properties: { accountId: { type: "string" } }, required: ["accountId"] } as any }],
};

for (const repeatIdAndName of [false, true]) {
  test(`pi-ai reads reasoning_content and tool-call deltas that repeat "type":"function"${repeatIdAndName ? ", the id and the name" : ""}`, async () => {
    const endpoint = await streamingEndpoint(toolCallChunks(repeatIdAndName));
    try {
      const message = await streamSimple(openAICompatibleModel("qwen3.8-flash-next", endpoint.baseUrl), context, { apiKey: "sk-qwen", maxRetries: 0 }).result();
      assert.equal(message.stopReason, "toolUse", message.errorMessage);
      const thinking = message.content.filter((block) => block.type === "thinking");
      assert.deepEqual(thinking.map((block) => block.thinking), ["Check the profile first."]);
      const toolCalls = message.content.filter((block) => block.type === "toolCall");
      assert.equal(toolCalls.length, 1);
      assert.deepEqual({ id: toolCalls[0]!.id, name: toolCalls[0]!.name, arguments: toolCalls[0]!.arguments }, { id: "call_1", name: "lookup_profile", arguments: { accountId: "a-1" } });
      assert.equal(message.content.some((block) => block.type === "text"), false);

      const [request] = endpoint.requests;
      assert.equal(endpoint.requests.length, 1);
      assert.equal(request!.url, "/v1/chat/completions");
      assert.equal(request!.headers.authorization, "Bearer sk-qwen");
      assert.equal(request!.body.model, "qwen3.8-flash-next");
      assert.equal(request!.body.messages[0].role, "system");
      assert.equal(typeof request!.body.max_tokens, "number");
      assert.ok(request!.body.max_tokens <= openAICompatibleMaxTokens);
      for (const absent of ["store", "max_completion_tokens", "reasoning_effort", "enable_thinking"]) {
        assert.equal(request!.body[absent], undefined, absent);
      }
    } finally {
      await endpoint.close();
    }
  });
}
