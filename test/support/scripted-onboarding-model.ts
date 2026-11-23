// TEST ONLY: an AI SDK mock model that plays the onboarding wizard from its prompt alone: the last user message and the
// tool results after it, one tool call per response. It never calls a real model.

import type { LanguageModelV4CallOptions, LanguageModelV4StreamPart } from "@ai-sdk/provider";
import { MockLanguageModelV4, convertArrayToReadableStream } from "ai/test";

type Call = { readonly name: string; readonly input: Record<string, unknown> };

/** The text of each user message, and the tool results after the last one, by tool name. */
function readPrompt(prompt: LanguageModelV4CallOptions["prompt"]): { userTexts: string[]; results: { name: string; output: string }[] } {
  const userTexts: string[] = [];
  let results: { name: string; output: string }[] = [];
  for (const message of prompt) {
    if (message.role === "user") {
      userTexts.push(message.content.map((part) => (part.type === "text" ? part.text : "")).join(""));
      results = [];
    } else if (message.role === "tool") {
      for (const part of message.content) {
        if (part.type === "tool-result") {
          const output = part.output as { type: string; value?: unknown };
          results.push({ name: part.toolName, output: typeof output.value === "string" ? output.value : JSON.stringify(output.value ?? "") });
        }
      }
    }
  }
  return { userTexts, results };
}

/** The onboarding procedure as a script: the next tool call, or the final answer. */
export function nextOnboardingStep(userTexts: readonly string[], results: readonly { name: string; output: string }[]): Call | { answer: string } {
  const last = userTexts.at(-1) ?? "";
  const done = (name: string) => results.some((result) => result.name === name);
  const plan: Call[] = [];
  if (last.startsWith("Onboard Dex Cloud account")) {
    plan.push({ name: "get_usage", input: {} });
    plan.push({ name: "post_onboarding_report", input: { headline: "Approved today; wants to build a support agent", insights: ["Purpose fits the Studio agent path: start from a template."] } });
    plan.push({ name: "schedule_check_in", input: { days: 2, reason: "first look after the welcome email" } });
  } else if (last.startsWith("[check-in ")) {
    if (/wrap up/.test(last)) {
      plan.push({ name: "reply_in_thread", input: { text: "Onboarding closed: the window ended." } });
      plan.push({ name: "finish_onboarding", input: { reason: "window_ended" } });
    } else {
      plan.push({ name: "post_report_update", input: { changes: "No activity since the first sign-in.", insights: ["Stalled before creating a project."] } });
      if (/calls for a follow-up/.test(last)) {
        plan.push({ name: "draft_nudge", input: { subject: "Your support agent on Dex Cloud", body: "Hi Ada,\n\nDescribe your support agent to the Studio agent and it drafts the first Flow.\n\nThe Dex Cloud team", why: "stalled before the first project" } });
      }
      plan.push({ name: "schedule_check_in", input: { days: 3, reason: "see whether the nudge helped" } });
    }
  } else if (/: send$/.test(last)) {
    plan.push({ name: "send_nudge", input: {} });
    if (done("send_nudge")) {
      const sent = results.find((result) => result.name === "send_nudge")!.output.startsWith("{");
      plan.push({ name: "reply_in_thread", input: { text: sent ? "Sent the nudge." : "The nudge was not sent." } });
    }
  } else if (/: skip$/.test(last)) {
    plan.push({ name: "reply_in_thread", input: { text: "Skipped this nudge." } });
  } else if (last.startsWith("[slack ")) {
    plan.push({ name: "draft_nudge", input: { subject: "Your support agent on Dex Cloud", body: "Hi Ada,\n\nShorter: open Studio and describe your agent.\n\nThe Dex Cloud team", why: "revised as asked" } });
  }
  return plan.find((call) => !done(call.name)) ?? { answer: "Done." };
}

const usage = { inputTokens: { total: 10, noCache: 10, cacheRead: 0, cacheWrite: 0 }, outputTokens: { total: 5, text: 5, reasoning: 0 } };

/** The mock model; `calls` counts its model calls. */
export function scriptedOnboardingModel(): MockLanguageModelV4 {
  return new MockLanguageModelV4({
    provider: "dex-test",
    modelId: "scripted-onboarding",
    doStream: async (options) => {
      const { userTexts, results } = readPrompt(options.prompt);
      const step = nextOnboardingStep(userTexts, results);
      const parts: LanguageModelV4StreamPart[] = [{ type: "stream-start", warnings: [] }];
      if ("answer" in step) {
        parts.push({ type: "text-start", id: "t" }, { type: "text-delta", id: "t", delta: step.answer }, { type: "text-end", id: "t" });
        parts.push({ type: "finish", usage, finishReason: { unified: "stop", raw: "stop" } });
      } else {
        parts.push({ type: "tool-call", toolCallId: `call-${userTexts.length}-${step.name}`, toolName: step.name, input: JSON.stringify(step.input) });
        parts.push({ type: "finish", usage, finishReason: { unified: "tool-calls", raw: "tool_use" } });
      }
      return { stream: convertArrayToReadableStream(parts) };
    },
  });
}
