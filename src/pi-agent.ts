// The pi harness: Dex's built-in agent loop runs the session, pi-ai makes each model call.
// Every model call and every tool call is its own Dex Step.

import type { Api, Model } from "@earendil-works/pi-ai";
import { getBuiltinModels, type BuiltinProvider } from "@earendil-works/pi-ai/providers/all";
import { createBuiltinAgentSessionFlow, functionTool, type ModelClient } from "@superdurable/dex-agents/builtin-loop";
import { createPiAiModelClient } from "@superdurable/dex-agents-pi";

import { openAICompatibleProvider, type PiModelSelection } from "./config.ts";
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

/** The context window and the output limit of an OpenAI-compatible model; pi-ai sends max_tokens with every request. */
export const openAICompatibleContextWindow = 131_072;
export const openAICompatibleMaxTokens = 16_384;

/**
 * A model of an OpenAI-compatible Chat Completions endpoint outside pi-ai's catalog. Its provider is no catalog
 * provider, so pi-ai streams it with its generic `openai-completions` client and never reads a provider key from the
 * environment. The compat settings are explicit rather than detected from the URL: no `store` field, the `system`
 * role, `max_tokens`, and no reasoning parameters, so the endpoint decides whether the model reasons. Streamed
 * `reasoning_content` becomes thinking, as with any OpenAI-compatible model.
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

/** The model client of the configured pi model. */
export function configuredPiModel(selection: PiModelSelection): ModelClient {
  return selection.kind === "openai-compatible"
    ? createPiAiModelClient(openAICompatibleModel(selection.modelId, selection.baseUrl), { apiKey: selection.apiKey })
    : piModel(selection.name, selection.apiKey);
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
