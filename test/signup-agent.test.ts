// The send caps of the connector tools: the app keeps each connector's declared bounded resend, and the connector
// write a tool makes itself in its Step, checked against the Connector SDK's own single-dispatch fence.

import assert from "node:assert/strict";
import test from "node:test";

import gmailManifest from "@superdurable/connector-google-gmail/dex-connector.json" with { type: "json" };
import { GmailMock } from "@superdurable/connector-google-gmail/mock";
import { PostThreadReplyDefinition } from "@superdurable/connector-slack";
import slackManifest from "@superdurable/connector-slack/dex-connector.json" with { type: "json" };
import { GeneratedMock } from "@superdurable/connector-slack/mock";
import { createCall, definitionMaxSends, type ConnectionRef } from "@superdurable/connectors-sdk";
import { Fence } from "@superdurable/connectors-sdk/singledispatch";
import type { AgentToolCall, AgentTurnContext } from "@superdurable/dex-agents";
import type { ConnectorToolSource } from "@superdurable/dex-agents/connectors";
import type { AsyncContext } from "@superdurable/dex";

import { createSignupConnectorTools, signupConnectorToolPolicies, type SignupServices } from "../src/signup-agent.ts";

const connection: ConnectionRef = { provider: "slack", name: "review-slack" };
const connectorTools = ["post_review", "post_draft", "reply_in_thread", "approve_account", "reject_account", "send_welcome_email"];

/** The cap a manifest declares for `operationId`. */
const declaredCap = (manifest: unknown, operationId: string): unknown =>
  (manifest as { factories: { operationId?: string; agent?: { maxSends?: unknown } }[] }).factories.find((entry) => entry.operationId === operationId)?.agent?.maxSends;

/** A second attempt of a Step whose first attempt left `marker` as its checkpoint. */
function retryContext(marker: () => unknown): AsyncContext {
  return {
    flowId: "signup-review-acct1",
    stepExecutionId: "step-1",
    attempt: 2,
    hasLastHeartbeatValue: () => true,
    getLastHeartbeatValue: () => marker(),
  } as unknown as AsyncContext;
}

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

test("send_welcome_email's outcome reply has Slack's declared cap: after its own unknown send it may read back and resend", () => {
  const operation = PostThreadReplyDefinition.operation;
  const slackCap = definitionMaxSends(PostThreadReplyDefinition);
  assert.equal(slackCap, 3);
  let callId = "";
  const reply = createCall(retryContext(() => ({ callId, attempt: 1, state: "claimed", sendCount: 1 })), connection, operation);
  callId = reply.id;
  assert.equal(reply.maxSendsOverride, undefined);
  assert.equal(new Fence(reply, 30_000, { maxSends: slackCap }).decide().kind, "readBackThenResendAllowed");
});

test("an outcome reply in a later attempt of send_welcome_email's Step finds the email's checkpoint and fails closed", () => {
  // The Step keeps one checkpoint: after the email's send in attempt 1, it is the Gmail call's marker. That is why the
  // tool posts nothing on a Gmail retry, and why a reply in a later attempt is not posted (confirmationPosted: false).
  const gmailCall = createCall(retryContext(() => undefined), { provider: "gmail", name: "welcome-gmail" }, { connectorId: "gmail", operationId: "sendMessage" });
  const reply = createCall(retryContext(() => ({ callId: gmailCall.id, attempt: 1, state: "claimed", providerId: "m-1", sendCount: 1 })), connection, PostThreadReplyDefinition.operation);
  assert.notEqual(reply.id, gmailCall.id);
  assert.equal(new Fence(reply, 30_000, { maxSends: definitionMaxSends(PostThreadReplyDefinition) }).decide().kind, "failClosed");
});
