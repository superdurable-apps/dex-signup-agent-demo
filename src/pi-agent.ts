// The pi harness: Dex's built-in agent loop runs the session, pi-ai makes each model call.
// Every model call and every tool call is its own Dex Step.

import type { Api, Context, Model } from "@earendil-works/pi-ai";
import { getBuiltinModels, type BuiltinProvider } from "@earendil-works/pi-ai/providers/all";
import { createBuiltinAgentSessionFlow, functionTool, type ModelClient } from "@superdurable/dex-agents/builtin-loop";
import { createPiAiModelClient } from "@superdurable/dex-agents-pi";

import { anthropicCompatibleProvider, openAICompatibleProvider, type PiModelSelection } from "./config.ts";
import type { ReviewSessionFlow } from "./review-state.ts";
import { createSignupConnectorTools, createSignupTools, signupConnectorToolPolicies, runSignupTool, signupAgentPrompt, toolJsonSchema, type SignupServices } from "./signup-agent.ts";

export const piHarnessLabel = "pi (Dex built-in agent loop with pi-ai)";

/**
 * A pi-ai model, `<provider>/<model ID>` of its built-in catalog, with the provider key; without one pi-ai reads the
 * provider key from the process environment.
 */
export function piModel(name: string, apiKey?: string): ModelClient {
  const [provider, modelId] = [name.slice(0, name.indexOf("/")), name.slice(name.indexOf("/") + 1)];
  const model = getBuiltinModels(provider as BuiltinProvider).find((candidate) => candidate.id === modelId) as Model<Api> | undefined;
  return createPiAiModelClient(model ?? (() => { throw new Error(`pi model ${name} is not in pi-ai's catalog`); })(), apiKey === undefined ? undefined : { apiKey });
}

/** The context window and the output limit of a custom endpoint's model, of either protocol; pi-ai sends max_tokens with every request. */
export const openAICompatibleContextWindow = 131_072;
export const openAICompatibleMaxTokens = 16_384;

/**
 * A model of an OpenAI-compatible Chat Completions endpoint outside pi-ai's catalog. Its provider is no catalog
 * provider, so pi-ai streams it with its generic `openai-completions` client and never reads a provider key from the
 * environment. The compat settings are explicit rather than detected from the URL: no `store` field, the `system`
 * role, `max_tokens`, and no reasoning parameters, so the endpoint decides whether the model reasons. Streamed
 * `reasoning_content` becomes thinking, which {@link configuredPiModel} leaves out of later requests.
 */
export function openAICompatibleModel(modelId: string, baseUrl: string): Model<"openai-completions"> {
  return {
    id: modelId,
    name: modelId,
    api: "openai-completions",
    provider: openAICompatibleProvider,
    baseUrl,
    reasoning: false,
    input: ["text"],
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    contextWindow: openAICompatibleContextWindow,
    maxTokens: openAICompatibleMaxTokens,
    compat: { supportsStore: false, supportsDeveloperRole: false, supportsReasoningEffort: false, maxTokensField: "max_tokens" },
  };
}

/**
 * The request context without the thinking of earlier responses. pi-ai replays streamed `reasoning_content` as a field
 * of the assistant messages it sends back, which an OpenAI-compatible endpoint may reject; the text, the tool calls and
 * the tool results stay. The session keeps the thinking it stored: only this request leaves it out.
 */
export function withoutReplayedThinking(context: Context): Context {
  return {
    ...context,
    messages: context.messages.map((message) =>
      message.role === "assistant" && message.content.some((block) => block.type === "thinking")
        ? { ...message, content: message.content.filter((block) => block.type !== "thinking") }
        : message,
    ),
  };
}

/**
 * A model of an Anthropic-compatible Messages endpoint outside pi-ai's catalog; pi-ai's Anthropic client posts to
 * `<baseUrl>/v1/messages`, and {@link configuredPiModel} sends the key as the `x-api-key` header. Its provider is no
 * catalog provider, so pi-ai reads no key and no Anthropic credentials from the environment. It asks for no thinking. Tools go without `eager_input_streaming` (the
 * fine-grained streaming beta header instead), since a compatible endpoint may reject unknown tool fields, and no
 * one-hour cache retention. Any thinking the endpoint returns is replayed with its signature, as the Messages API
 * expects within a tool loop.
 */
export function anthropicCompatibleModel(modelId: string, baseUrl: string): Model<"anthropic-messages"> {
  return {
    id: modelId,
    name: modelId,
    api: "anthropic-messages",
    provider: anthropicCompatibleProvider,
    baseUrl,
    reasoning: false,
    input: ["text"],
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    contextWindow: openAICompatibleContextWindow,
    maxTokens: openAICompatibleMaxTokens,
    compat: { supportsEagerToolInputStreaming: false, supportsLongCacheRetention: false },
  };
}

/** The model client of the configured pi model. */
export function configuredPiModel(selection: PiModelSelection): ModelClient {
  switch (selection.kind) {
    case "openai-compatible":
      return createPiAiModelClient(openAICompatibleModel(selection.modelId, selection.baseUrl), { apiKey: selection.apiKey, prepareRequest: withoutReplayedThinking });
    case "anthropic-compatible":
      // The key goes as the x-api-key header, not as pi-ai's apiKey: pi-ai treats an apiKey that contains `sk-ant-oat`
      // as a Claude OAuth token and would send it as a Bearer token with Claude Code identity headers.
      return createPiAiModelClient(anthropicCompatibleModel(selection.modelId, selection.baseUrl), { headers: { "x-api-key": selection.apiKey } });
    case "catalog":
      return piModel(selection.name, selection.apiKey);
  }
}

export function createPiSignupAgent(services: SignupServices, model: ModelClient): ReviewSessionFlow {
  return createBuiltinAgentSessionFlow(
    {
      name: "signup-review",
      instructions: signupAgentPrompt,
      model,
      tools: createSignupTools(services).map((tool) =>
        functionTool({
          name: tool.name,
          description: tool.description,
          parameters: toolJsonSchema(tool.schema),
          effect: tool.effect,
          // Approval comes from the reviewers in Slack, checked by the tools themselves.
          requiresApproval: false,
          execute: (args, context) => runSignupTool(tool, args, { sessionId: context.flowId, callKey: context.idempotencyKey, signal: context.cancellationSignal }),
        }),
      ),
      toolSources: [createSignupConnectorTools(services)],
      toolPolicies: signupConnectorToolPolicies,
    },
    "SignupReviewPiAgentFlow",
  );
}
