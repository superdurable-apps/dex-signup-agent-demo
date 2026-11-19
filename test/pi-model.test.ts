// The pi model selection of the configuration, and the OpenAI-compatible model against a local streaming endpoint.

import assert from "node:assert/strict";
import { createServer, type IncomingHttpHeaders } from "node:http";
import type { AddressInfo } from "node:net";
import { readFileSync } from "node:fs";
import test from "node:test";

import type { Context } from "@earendil-works/pi-ai";
import { streamSimple } from "@earendil-works/pi-ai/compat";
import type { ModelClient, ModelInputItem, ModelRequest, ModelResponse } from "@superdurable/dex-agents/builtin-loop";
import { createPiAiModelClient } from "@superdurable/dex-agents-pi";
import { parse } from "yaml";

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

test("the legacy openai-compatible/<model ID> with a base URL selects the endpoint and its key, and needs no ANTHROPIC_API_KEY", () => {
  const environment = { SIGNUP_AGENT_PI_MODEL: "openai-compatible/qwen3.8-flash-next", SIGNUP_AGENT_PI_BASE_URL: "https://llm.example.com/v1", SIGNUP_AGENT_PI_API_KEY: "sk-qwen" };
  assert.deepEqual(configOf(environment).piModel, { kind: "openai-compatible", modelId: "qwen3.8-flash-next", baseUrl: "https://llm.example.com/v1", apiKey: "sk-qwen" });
  // A model ID may itself contain a slash.
  assert.deepEqual(configOf({ ...environment, SIGNUP_AGENT_PI_MODEL: "openai-compatible/Qwen/Qwen3-8B" }).piModel, { kind: "openai-compatible", modelId: "Qwen/Qwen3-8B", baseUrl: "https://llm.example.com/v1", apiKey: "sk-qwen" });
  assert.throws(() => configOf({ ...environment, SIGNUP_AGENT_PI_API_KEY: "" }), /SIGNUP_AGENT_PI_API_KEY is required/);
  assert.throws(() => configOf({ ...environment, SIGNUP_AGENT_PI_BASE_URL: "" }), /needs SIGNUP_AGENT_PI_BASE_URL/);
});

test("SIGNUP_AGENT_PI_BASE_URL must be an http(s) base URL with a host, without a query, a fragment or /chat/completions", () => {
  const environment = { SIGNUP_AGENT_PI_MODEL: "openai-compatible/qwen3.8-flash-next", SIGNUP_AGENT_PI_API_KEY: "sk-qwen" };
  const baseUrlOf = (url: string) => (configOf({ ...environment, SIGNUP_AGENT_PI_BASE_URL: url }).piModel as { baseUrl: string }).baseUrl;
  for (const valid of ["https://llm.example.com/v1", "http://127.0.0.1:8000/v1", "https://llm.example.com", "https://llm.example.com/compatible-mode/v1/"]) {
    assert.equal(baseUrlOf(valid), valid);
  }
  const refusals: [string, RegExp][] = [
    ["llm.example.com/v1", /is not a URL/],
    ["https://?x", /is not a URL/],
    ["https://#x", /is not a URL/],
    ["https://host space/v1", /is not a URL/],
    ["ftp://llm.example.com/v1", /must be an http\(s\) URL with a host/],
    ["https://llm.example.com/v1?api-version=1", /must not have a query or a fragment/],
    ["https://llm.example.com/v1?", /must not have a query or a fragment/],
    ["https://llm.example.com/v1#models", /must not have a query or a fragment/],
    ["https://llm.example.com/v1/chat/completions", /must not end in \/chat\/completions/],
    ["https://llm.example.com/v1/chat/completions/", /must not end in \/chat\/completions/],
  ];
  for (const [url, error] of refusals) {
    assert.throws(() => baseUrlOf(url), (thrown: Error) => error.test(thrown.message) && /give the base URL only/.test(thrown.message), url);
  }
});

test("with SIGNUP_AGENT_PI_BASE_URL, SIGNUP_AGENT_PI_MODEL is the endpoint's model ID as it is", () => {
  const endpoint = { SIGNUP_AGENT_PI_BASE_URL: "https://llm.example.com/v1", SIGNUP_AGENT_PI_API_KEY: "sk-qwen" };
  const modelIdOf = (model: string) => configOf({ ...endpoint, SIGNUP_AGENT_PI_MODEL: model }).piModel;
  for (const [model, modelId] of [
    ["qwen3.8-flash-next", "qwen3.8-flash-next"],
    ["  qwen3.8-flash-next ", "qwen3.8-flash-next"],
    // Slashes stay part of the ID when the prefix is no pi-ai catalog provider.
    ["meta-llama/Llama-3", "meta-llama/Llama-3"],
    ["qwen/qwen3", "qwen/qwen3"],
    // The legacy prefix, as the Live settings have it.
    ["openai-compatible/qwen3.8-flash-next", "qwen3.8-flash-next"],
    // The legacy prefix is the way to write an endpoint model ID that starts with a catalog provider.
    ["openai-compatible/openai/gpt-oss-120b", "openai/gpt-oss-120b"],
  ]) {
    assert.deepEqual(modelIdOf(model!), { kind: "openai-compatible", modelId, baseUrl: "https://llm.example.com/v1", apiKey: "sk-qwen" }, model);
  }
  // An ANTHROPIC_API_KEY in the environment does not matter here.
  assert.equal(configOf({ ...endpoint, SIGNUP_AGENT_PI_MODEL: "qwen3.8-flash-next", ANTHROPIC_API_KEY: "sk-ant" }).piModel?.kind, "openai-compatible");
});

test("inconsistent pi model settings are refused at startup with what to set", () => {
  const endpoint = { SIGNUP_AGENT_PI_BASE_URL: "https://llm.example.com/v1", SIGNUP_AGENT_PI_API_KEY: "sk-qwen" };
  const refusals: [Record<string, string>, RegExp][] = [
    // A base URL with a catalog model.
    [{ ...endpoint, SIGNUP_AGENT_PI_MODEL: "anthropic/claude-opus-5-5", ANTHROPIC_API_KEY: "sk-ant" }, /names the pi-ai catalog provider anthropic, but SIGNUP_AGENT_PI_BASE_URL is set: unset SIGNUP_AGENT_PI_BASE_URL .* or write the endpoint's model ID as openai-compatible\/anthropic\/claude-opus-5-5/],
    [{ ...endpoint, SIGNUP_AGENT_PI_MODEL: "openai/gpt-oss-120b" }, /names the pi-ai catalog provider openai/],
    // A base URL without a model, or with an empty one.
    [{ ...endpoint }, /SIGNUP_AGENT_PI_MODEL is required with SIGNUP_AGENT_PI_BASE_URL/],
    [{ ...endpoint, SIGNUP_AGENT_PI_MODEL: "  " }, /SIGNUP_AGENT_PI_MODEL is required with SIGNUP_AGENT_PI_BASE_URL/],
    [{ ...endpoint, SIGNUP_AGENT_PI_MODEL: "openai-compatible/" }, /SIGNUP_AGENT_PI_MODEL has no model ID/],
    // The legacy prefix without a base URL.
    [{ SIGNUP_AGENT_PI_MODEL: "openai-compatible/qwen3.8-flash-next", SIGNUP_AGENT_PI_API_KEY: "sk-qwen" }, /openai-compatible\/qwen3.8-flash-next needs SIGNUP_AGENT_PI_BASE_URL/],
    // No base URL and no catalog provider, as when the owner typed just "openai".
    [{ SIGNUP_AGENT_PI_MODEL: "openai" }, /SIGNUP_AGENT_PI_MODEL openai is not <provider>\/<model ID> of pi-ai's catalog.*set SIGNUP_AGENT_PI_BASE_URL/],
    [{ SIGNUP_AGENT_PI_MODEL: "qwen3.8-flash-next" }, /set SIGNUP_AGENT_PI_BASE_URL/],
    [{ SIGNUP_AGENT_PI_MODEL: "meta-llama/Llama-3" }, /set SIGNUP_AGENT_PI_BASE_URL/],
    // A catalog provider with a model its catalog lacks.
    [{ SIGNUP_AGENT_PI_MODEL: "anthropic/no-such-model", ANTHROPIC_API_KEY: "sk-ant" }, /pi-ai's catalog has no model no-such-model of anthropic/],
    [{ SIGNUP_AGENT_PI_MODEL: "anthropic/", ANTHROPIC_API_KEY: "sk-ant" }, /pi-ai's catalog has no model/],
  ];
  for (const [environment, error] of refusals) {
    assert.throws(() => configOf(environment), error, JSON.stringify(environment));
  }
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

/**
 * A Chat Completions endpoint that records each request and streams the chunks the responder gives for it, or answers
 * 400 when the responder gives none.
 */
async function streamingEndpoint(respond: (body: string, requestNumber: number) => readonly object[] | undefined) {
  const requests: CapturedRequest[] = [];
  const server = createServer((request, response) => {
    let body = "";
    request.on("data", (piece) => (body += piece));
    request.on("end", () => {
      requests.push({ url: request.url, headers: request.headers, body: JSON.parse(body) });
      const chunks = respond(body, requests.length);
      if (chunks === undefined) {
        response.writeHead(400, { "content-type": "application/json" });
        response.end(JSON.stringify({ error: { message: "unknown field reasoning_content", type: "invalid_request_error" } }));
        return;
      }
      response.writeHead(200, { "content-type": "text/event-stream" });
      for (const chunk of chunks) {
        // Anthropic events name themselves in an `event:` line; Chat Completions chunks have none.
        const event = (chunk as { type?: unknown }).type;
        response.write(`${typeof event === "string" ? `event: ${event}\n` : ""}data: ${JSON.stringify(chunk)}\n\n`);
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
    const endpoint = await streamingEndpoint(() => toolCallChunks(repeatIdAndName));
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

const modelRequest = (inputItems: readonly ModelInputItem[], attempt: number): ModelRequest => ({
  instructions: "You review sign-ups.",
  inputItems,
  tools: [{ name: "lookup_profile", description: "Looks up a profile.", parameters: { type: "object", properties: { accountId: { type: "string" } }, required: ["accountId"] } }],
  toolChoice: "auto",
  settings: {},
  metadata: { flowId: "signup-review-a-1", turnId: "turn-1", stepExecutionId: `step-${attempt}`, attempt: 1 },
  cancellationSignal: new AbortController().signal,
});
const discard = { writeTextDelta: () => {}, writeReasoningDelta: () => {} };
const answerChunks = [chunk({ role: "assistant", content: "Account a-1 looks fine." }), chunk({}, "stop")];

/** One tool loop: the first call streams thinking and a tool call, the second sends the tool result back. */
async function toolLoop(client: ModelClient): Promise<{ first: ModelResponse; second: ModelResponse | Error }> {
  const user: ModelInputItem = { type: "user_text", text: "Review account a-1." };
  const first = await client.callModel(modelRequest([user], 1), discard);
  assert.deepEqual(first.toolCalls.map((call) => call.toolName), ["lookup_profile"]);
  const result: ModelInputItem = { type: "tool_result", toolCallId: first.toolCalls[0]!.toolCallId, toolName: "lookup_profile", output: '{"github":"octocat"}' };
  try {
    return { first, second: await client.callModel(modelRequest([user, ...first.outputItems, result], 2), discard) };
  } catch (error) {
    return { first, second: error as Error };
  }
}

test("the OpenAI-compatible model leaves earlier thinking out of the request that returns the tool result", async () => {
  // The endpoint rejects any request that carries reasoning_content, as some OpenAI-compatible endpoints do.
  const respond = (body: string, requestNumber: number) => (body.includes("reasoning_content") ? undefined : requestNumber === 1 ? toolCallChunks(false) : answerChunks);
  const endpoint = await streamingEndpoint(respond);
  try {
    const { first, second } = await toolLoop(configuredPiModel({ kind: "openai-compatible", modelId: "qwen3.8-flash-next", baseUrl: endpoint.baseUrl, apiKey: "sk-qwen" }));
    // The session still stores the thinking; only the request leaves it out.
    assert.ok(first.outputItems.some((item) => item.item.includes("Check the profile first.")));
    assert.ok(!(second instanceof Error), String(second));
    assert.equal(second.text, "Account a-1 looks fine.");
    const followUp = endpoint.requests[1]!.body;
    assert.equal(JSON.stringify(followUp).includes("reasoning_content"), false);
    const assistant = followUp.messages.find((message: { role: string }) => message.role === "assistant");
    assert.deepEqual(assistant.tool_calls.map((call: { id: string; function: { name: string; arguments: string } }) => [call.id, call.function.name, JSON.parse(call.function.arguments)]), [["call_1", "lookup_profile", { accountId: "a-1" }]]);
    const tool = followUp.messages.find((message: { role: string }) => message.role === "tool");
    assert.deepEqual([tool.tool_call_id, tool.content], ["call_1", '{"github":"octocat"}']);
  } finally {
    await endpoint.close();
  }

  // Without the hook, pi-ai replays the thinking and the same endpoint rejects the follow-up.
  const control = await streamingEndpoint(respond);
  try {
    const { second } = await toolLoop(createPiAiModelClient(openAICompatibleModel("qwen3.8-flash-next", control.baseUrl), { apiKey: "sk-qwen" }));
    assert.ok(second instanceof Error);
    assert.ok(JSON.stringify(control.requests[1]!.body).includes("reasoning_content"));
  } finally {
    await control.close();
  }
});

test("SIGNUP_AGENT_PI_PROTOCOL picks the custom endpoint's API, openai by default", () => {
  const endpoint = { SIGNUP_AGENT_PI_BASE_URL: "https://llm.example.com/v1", SIGNUP_AGENT_PI_API_KEY: "sk-custom", SIGNUP_AGENT_PI_MODEL: "qwen3.8-flash-next" };
  const openai = { kind: "openai-compatible", modelId: "qwen3.8-flash-next", baseUrl: "https://llm.example.com/v1", apiKey: "sk-custom" };
  assert.deepEqual(configOf(endpoint).piModel, openai);
  assert.deepEqual(configOf({ ...endpoint, SIGNUP_AGENT_PI_PROTOCOL: "" }).piModel, openai);
  assert.deepEqual(configOf({ ...endpoint, SIGNUP_AGENT_PI_PROTOCOL: "openai" }).piModel, openai);
  const anthropic = { ...endpoint, SIGNUP_AGENT_PI_PROTOCOL: "anthropic", SIGNUP_AGENT_PI_BASE_URL: "https://llm.example.com/anthropic" };
  assert.deepEqual(configOf(anthropic).piModel, { kind: "anthropic-compatible", modelId: "qwen3.8-flash-next", baseUrl: "https://llm.example.com/anthropic", apiKey: "sk-custom" });
  // With the anthropic protocol chosen, a catalog-like prefix is part of the endpoint's model ID.
  assert.deepEqual(configOf({ ...anthropic, SIGNUP_AGENT_PI_MODEL: "anthropic/claude-sonnet" }).piModel, { kind: "anthropic-compatible", modelId: "anthropic/claude-sonnet", baseUrl: "https://llm.example.com/anthropic", apiKey: "sk-custom" });

  const refusals: [Record<string, string>, RegExp][] = [
    [{ SIGNUP_AGENT_PI_PROTOCOL: "openai", ANTHROPIC_API_KEY: "sk-ant" }, /SIGNUP_AGENT_PI_PROTOCOL applies only to a custom endpoint: set SIGNUP_AGENT_PI_BASE_URL/],
    [{ SIGNUP_AGENT_PI_PROTOCOL: "anthropic", ANTHROPIC_API_KEY: "sk-ant" }, /SIGNUP_AGENT_PI_PROTOCOL applies only to a custom endpoint/],
    [{ ...endpoint, SIGNUP_AGENT_PI_PROTOCOL: "gemini" }, /SIGNUP_AGENT_PI_PROTOCOL must be openai or anthropic/],
    [{ ...endpoint, SIGNUP_AGENT_PI_PROTOCOL: "Anthropic" }, /SIGNUP_AGENT_PI_PROTOCOL must be openai or anthropic/],
    [{ ...anthropic, SIGNUP_AGENT_PI_MODEL: "openai-compatible/qwen3.8-flash-next" }, /is the OpenAI-compatible form, but SIGNUP_AGENT_PI_PROTOCOL is anthropic/],
    [{ ...anthropic, SIGNUP_AGENT_PI_API_KEY: "" }, /SIGNUP_AGENT_PI_API_KEY is required/],
    [{ ...anthropic, SIGNUP_AGENT_PI_BASE_URL: "https://llm.example.com/anthropic/v1/messages" }, /must not end in \/messages: with SIGNUP_AGENT_PI_PROTOCOL=anthropic the client appends \/v1\/messages/],
  ];
  for (const [environment, error] of refusals) {
    assert.throws(() => configOf(environment), error, JSON.stringify(environment));
  }
});

/** Anthropic Messages stream events: signed thinking, then a tool call whose input arrives in pieces; or a text answer. */
const anthropicToolCallEvents = [
  { type: "message_start", message: { id: "msg_1", type: "message", role: "assistant", model: "claude-compatible", content: [], stop_reason: null, usage: { input_tokens: 40, output_tokens: 1 } } },
  { type: "content_block_start", index: 0, content_block: { type: "thinking", thinking: "", signature: "" } },
  { type: "content_block_delta", index: 0, delta: { type: "thinking_delta", thinking: "Check the " } },
  { type: "content_block_delta", index: 0, delta: { type: "thinking_delta", thinking: "profile first." } },
  { type: "content_block_delta", index: 0, delta: { type: "signature_delta", signature: "sig-abc123" } },
  { type: "content_block_stop", index: 0 },
  { type: "content_block_start", index: 1, content_block: { type: "tool_use", id: "toolu_1", name: "lookup_profile", input: {} } },
  ...['{"account', 'Id":"a-1"', "}"].map((piece) => ({ type: "content_block_delta", index: 1, delta: { type: "input_json_delta", partial_json: piece } })),
  { type: "content_block_stop", index: 1 },
  { type: "message_delta", delta: { stop_reason: "tool_use", stop_sequence: null }, usage: { output_tokens: 12 } },
  { type: "message_stop" },
];
const anthropicAnswerEvents = [
  { type: "message_start", message: { id: "msg_2", type: "message", role: "assistant", model: "claude-compatible", content: [], stop_reason: null, usage: { input_tokens: 60, output_tokens: 1 } } },
  { type: "content_block_start", index: 0, content_block: { type: "text", text: "" } },
  { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "Account a-1 looks fine." } },
  { type: "content_block_stop", index: 0 },
  { type: "message_delta", delta: { stop_reason: "end_turn", stop_sequence: null }, usage: { output_tokens: 6 } },
  { type: "message_stop" },
];

// A plain key, and a key that pi-ai would take for a Claude OAuth token if it were passed as pi-ai's apiKey.
for (const apiKey of ["sk-custom", "sk-ant-oat01-custom"]) {
  test(`the Anthropic-compatible model calls <base URL>/v1/messages with the key ${apiKey} as x-api-key, never ANTHROPIC_API_KEY`, async () => {
    const saved = { ANTHROPIC_API_KEY: process.env.ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN: process.env.ANTHROPIC_AUTH_TOKEN };
    process.env.ANTHROPIC_API_KEY = "sk-ant-from-environment";
    process.env.ANTHROPIC_AUTH_TOKEN = "token-from-environment";
    const endpoint = await streamingEndpoint((_body, requestNumber) => (requestNumber === 1 ? anthropicToolCallEvents : anthropicAnswerEvents));
    try {
      const baseUrl = endpoint.baseUrl.replace(/\/v1$/, "/anthropic");
      const environment = { SIGNUP_AGENT_PI_BASE_URL: baseUrl, SIGNUP_AGENT_PI_PROTOCOL: "anthropic", SIGNUP_AGENT_PI_MODEL: "claude-compatible", SIGNUP_AGENT_PI_API_KEY: apiKey };
      const { first, second } = await toolLoop(configuredPiModel(configOf(environment).piModel!));
      assert.deepEqual(first.toolCalls.map((call) => [call.toolCallId, call.toolName, JSON.parse(call.arguments)]), [["toolu_1", "lookup_profile", { accountId: "a-1" }]]);
      assert.ok(!(second instanceof Error), String(second));
      assert.equal(second.text, "Account a-1 looks fine.");

      assert.equal(endpoint.requests.length, 2);
      for (const request of endpoint.requests) {
        // pi-ai sends every Anthropic request through the SDK's beta Messages API, which adds ?beta=true.
        assert.equal(request.url, "/anthropic/v1/messages?beta=true");
        assert.equal(request.headers["x-api-key"], apiKey);
        assert.equal(request.headers.authorization, undefined);
        assert.equal(JSON.stringify(request.headers).includes("from-environment"), false);
        // No Claude Code identity and no OAuth betas, which pi-ai adds for an OAuth token.
        assert.equal(request.headers["x-app"], undefined);
        assert.equal(String(request.headers["user-agent"]).includes("claude-cli"), false);
        assert.equal(/oauth-|claude-code-/.test(String(request.headers["anthropic-beta"] ?? "")), false);
        assert.equal(request.body.model, "claude-compatible");
        assert.equal(request.body.thinking, undefined);
        assert.equal(JSON.stringify(request.body.tools).includes("eager_input_streaming"), false);
      }
      // The second request carries the signed thinking and the tool call back, then the tool result.
      type Block = { type: string; id?: string; tool_use_id?: string; thinking?: string; signature?: string };
      const messages = endpoint.requests[1]!.body.messages as { role: string; content: string | Block[] }[];
      const blocksOf = (role: string) => messages.filter((message) => message.role === role && Array.isArray(message.content)).flatMap((message) => message.content as Block[]);
      assert.deepEqual(
        blocksOf("assistant").map((block) => (block.type === "thinking" ? { type: block.type, thinking: block.thinking, signature: block.signature } : { type: block.type, id: block.id })),
        [
          { type: "thinking", thinking: "Check the profile first.", signature: "sig-abc123" },
          { type: "tool_use", id: "toolu_1" },
        ],
      );
      assert.ok(blocksOf("user").some((block) => block.type === "tool_result" && block.tool_use_id === "toolu_1"));
    } finally {
      await endpoint.close();
      for (const [name, value] of Object.entries(saved)) {
        if (value === undefined) delete process.env[name];
        else process.env[name] = value;
      }
    }
  });
}

// dex-app.yaml's application.requiredOneOf is what Setup and Publish check; each of its groups, set alone, must be a
// model the application starts with, so a publish that passes the check never crashes at start.
test("each group of the manifest's Model requirement alone configures a model, and no group means none", () => {
  const manifest = parse(readFileSync(new URL("../dex-app.yaml", import.meta.url), "utf8")) as {
    application: { environment: { name: string; required?: boolean; secret?: boolean; minLength?: number }[]; requiredOneOf: { label: string; groups: string[][] }[] };
  };
  const { environment, requiredOneOf } = manifest.application;
  assert.equal(requiredOneOf.length, 1);
  const { label, groups } = requiredOneOf[0]!;
  assert.equal(label, "Model");
  for (const name of groups.flat()) {
    const declaration = environment.find((entry) => entry.name === name);
    assert.ok(declaration, `${name} is declared`);
    assert.notEqual(declaration.required, true, `${name} is optional: the requirement decides when it is needed`);
    assert.equal(declaration.minLength, 1, `${name} has minLength 1`);
  }
  for (const group of groups) {
    const environmentOf = Object.fromEntries(group.map((name) => [name, name.endsWith("BASE_URL") ? "https://llm.example.com/v1" : name.endsWith("MODEL") ? "qwen3.8-flash-next" : "sk-test"]));
    assert.doesNotThrow(() => configOf(environmentOf), `group ${group.join(", ")}`);
  }
  assert.throws(() => configOf({}), /ANTHROPIC_API_KEY is required/);
});
