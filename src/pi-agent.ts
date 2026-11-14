// The pi harness: Dex's built-in agent loop runs the session, pi-ai makes each model call.
// Every model call and every tool call is its own Dex Step.

import type { Api, Model } from "@earendil-works/pi-ai";
import { getBuiltinModels, type BuiltinProvider } from "@earendil-works/pi-ai/providers/all";
import { createBuiltinAgentSessionFlow, functionTool, type ModelClient } from "@superdurable/dex-agents/builtin-loop";
import { createPiAiModelClient } from "@superdurable/dex-agents-pi";

import type { ReviewSessionFlow } from "./review-state.ts";
import { createSignupConnectorTools, createSignupTools, runSignupTool, signupAgentPrompt, toolJsonSchema, type SignupServices } from "./signup-agent.ts";

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
    },
    "SignupReviewPiAgentFlow",
  );
}
