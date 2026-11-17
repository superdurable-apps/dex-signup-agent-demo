// The send caps of the connector tools: the app keeps each connector's declared bounded resend. The connectors' fence
// behaviour itself is in test/connector-fences.test.ts.

import assert from "node:assert/strict";
import test from "node:test";

import gmailManifest from "@superdurable/connector-google-gmail/dex-connector.json" with { type: "json" };
import { GmailMock } from "@superdurable/connector-google-gmail/mock";
import slackManifest from "@superdurable/connector-slack/dex-connector.json" with { type: "json" };
import { GeneratedMock } from "@superdurable/connector-slack/mock";
import type { AgentToolCall, AgentTurnContext } from "@superdurable/dex-agents";
import type { ConnectorToolSource } from "@superdurable/dex-agents/connectors";

import { confirmationStateOf, createSignupConnectorTools, signupConnectorToolPolicies, type SignupServices } from "../src/signup-agent.ts";

const connectorTools = ["post_review", "post_draft", "reply_in_thread", "approve_account", "reject_account", "send_welcome_email"];

/** The cap a manifest declares for `operationId`. */
const declaredCap = (manifest: unknown, operationId: string): unknown =>
  (manifest as { factories: { operationId?: string; agent?: { maxSends?: unknown } }[] }).factories.find((entry) => entry.operationId === operationId)?.agent?.maxSends;

test("the connector tools' session policies set no send cap, only no approval", () => {
  assert.deepEqual(Object.keys(signupConnectorToolPolicies).sort(), [...connectorTools].sort());
  for (const [name, policy] of Object.entries(signupConnectorToolPolicies)) {
    assert.equal(policy.maxSends, undefined, `${name} keeps the connector's declared cap`);
    assert.equal(policy.requiresApproval, false, `${name} asks for no Dex approval`);
  }
});

test("each connector tool call carries the connector's declared cap of 3", () => {
  assert.equal(declaredCap(slackManifest, "postChannelMessage"), 3);
  assert.equal(declaredCap(slackManifest, "postThreadReply"), 3);
  assert.equal(declaredCap(gmailManifest, "sendMessage"), 3);
  const services = { slack: new GeneratedMock("review-slack").connection(), gmail: new GmailMock("welcome-gmail").connection(), slackChannelId: "C1" } as unknown as SignupServices;
  const source = createSignupConnectorTools(services) as ConnectorToolSource;
  const turn = { toolPolicies: signupConnectorToolPolicies } as unknown as AgentTurnContext;
  for (const [index, name] of connectorTools.entries()) {
    // The connector source reads only the call's name and arguments here, not its listed definition.
    const call = { toolCallId: `call-${index}`, toolName: name, arguments: "{}" } as unknown as Parameters<ConnectorToolSource["deferredToolCall"]>[1];
    const deferred = source.deferredToolCall(turn, call);
    // Dex Agents passes this pinned cap to the connector as the call's maxSendsOverride on every attempt.
    assert.deepEqual(JSON.parse(deferred.toolSourceReference ?? "{}"), { tool: name, maxSends: 3 }, name);
    assert.equal(deferred.policy.requiresApproval, false, name);
    const options = source.toolCallStepOptions({ toolName: name, toolSourceReference: deferred.toolSourceReference } as unknown as AgentToolCall);
    assert.ok((options?.boundedResendExecuteRetry?.maximumAttempts ?? 0) >= 3, `${name}'s tool Step gets an attempt per send`);
  }
});

test("a decision tool's note is posted, not posted, or unknown, from the Slack attempt", () => {
  const failure = { kind: "TRANSPORT", provider: "slack", operation: "postThreadReply", message: "x" };
  assert.equal(confirmationStateOf({ kind: "branch", branch: "sent" }), "posted");
  // Slack selects these only when nothing of the call went out.
  assert.equal(confirmationStateOf({ kind: "branch", branch: "providerRejected", failure }), "notPosted");
  assert.equal(confirmationStateOf({ kind: "branch", branch: "defect", failure }), "notPosted");
  // A retry may follow a lost answer, and an uncertain post may be in the thread.
  assert.equal(confirmationStateOf({ kind: "retry", failure }), "unknown");
  assert.equal(confirmationStateOf({ kind: "uncertain", branch: "uncertain", failure }), "unknown");
});
