// The onboarding wizard's harness: a Vercel AI SDK agent on the Dex Vercel AI adapter. Every model call is a RunModel
// Step and every tool call its own Dex Step, so a crash reruns only the Step that was running.

import { createAnthropic } from "@ai-sdk/anthropic";
import { createOpenAICompatible } from "@ai-sdk/openai-compatible";
import { createVercelAiAgentSessionFlow, getAgentToolExecutionScope } from "@superdurable/dex-agents-vercel-ai";
import { hasToolCall, isStepCount, tool, type LanguageModel, type ToolSet } from "ai";

import {
  createOnboardingConnectorTools,
  createOnboardingTools,
  onboardingAgentPrompt,
  onboardingConnectorToolPolicies,
  runOnboardingTool,
  type OnboardingServices,
} from "./onboarding-agent.ts";
import type { OnboardingSessionFlow } from "./onboarding-state.ts";

export const vercelAiHarnessLabel = "Vercel AI SDK on Dex";
/** The onboarding session's Flow type; never rename it while onboardings are open. */
export const onboardingFlowType = "OnboardingWizardVercelAiFlow";

/** ONBOARDING_AGENT_*: an Anthropic model with its key, or a model of an OpenAI-compatible endpoint. */
export type OnboardingModelSelection =
  | { readonly kind: "anthropic"; readonly modelId: string; readonly apiKey: string }
  | { readonly kind: "openai-compatible"; readonly modelId: string; readonly baseUrl: string; readonly apiKey: string };

export function onboardingModel(selection: OnboardingModelSelection): LanguageModel {
  if (selection.kind === "anthropic") {
    return createAnthropic({ apiKey: selection.apiKey })(selection.modelId);
  }
  return createOpenAICompatible({ name: "onboarding-endpoint", baseURL: selection.baseUrl, apiKey: selection.apiKey })(selection.modelId);
}

/** The onboarding session Flow. Build it the same way in every process: a tool Step may run in another Worker. */
export function createVercelAiOnboardingAgent(services: OnboardingServices, model: LanguageModel): OnboardingSessionFlow {
  const tools: ToolSet = {};
  const toolPolicies: Record<string, { effect: "read" | "idempotent_write"; requiresApproval: false }> = {};
  for (const definition of createOnboardingTools(services)) {
    tools[definition.name] = tool({
      description: definition.description,
      inputSchema: definition.schema,
      execute: async (input: unknown, options: { abortSignal?: AbortSignal }) =>
        runOnboardingTool(definition, input, { sessionId: getAgentToolExecutionScope()?.flowId, signal: options.abortSignal ?? new AbortController().signal }),
    });
    toolPolicies[definition.name] = { effect: definition.effect as "read" | "idempotent_write", requiresApproval: false };
  }
  return createVercelAiAgentSessionFlow(
    {
      model,
      instructions: onboardingAgentPrompt,
      tools,
      toolSources: [createOnboardingConnectorTools(services)],
      toolPolicies: { ...toolPolicies, ...onboardingConnectorToolPolicies },
      callSettings: { maxOutputTokens: 4_096 },
      // The turn ends right after finish_onboarding, with no further model call: a model that ignores "call no more
      // tools" cannot post after the onboarding ended (seen live on 2026-11-23).
      stopWhen: [isStepCount(20), hasToolCall("finish_onboarding")],
    },
    onboardingFlowType,
  ) as unknown as OnboardingSessionFlow;
}
