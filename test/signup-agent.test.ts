// The send cap of the connector writes a tool makes itself in its Step, checked against the Connector SDK's own
// single-dispatch fence: after an attempt whose send may have gone out, a retry only reads back.

import assert from "node:assert/strict";
import test from "node:test";

import { createCall, definitionMaxSends, type ConnectionRef } from "@superdurable/connectors-sdk";
import { Fence } from "@superdurable/connectors-sdk/singledispatch";
import { PostThreadReplyDefinition } from "@superdurable/connector-slack";
import type { AsyncContext } from "@superdurable/dex";

import { singleSendCall } from "../src/signup-agent.ts";

const connection: ConnectionRef = { provider: "slack", name: "review-slack" };

/** The second attempt of a Step whose first attempt claimed one send and then ended with an unknown outcome. */
function retryContext(callId: () => string): AsyncContext {
  return {
    flowId: "signup-review-acct1",
    stepExecutionId: "step-1",
    attempt: 2,
    hasLastHeartbeatValue: () => true,
    getLastHeartbeatValue: () => ({ callId: callId(), attempt: 1, state: "claimed", sendCount: 1 }),
  } as unknown as AsyncContext;
}

test("send_welcome_email's outcome reply never gets a second send after an unknown outcome", () => {
  const operation = PostThreadReplyDefinition.operation;
  const slackCap = definitionMaxSends(PostThreadReplyDefinition);
  assert.ok(slackCap >= 2, "Slack's postThreadReply declares a bounded resend");

  let callId = "";
  const capped = singleSendCall(retryContext(() => callId), connection, operation);
  callId = capped.id;
  assert.equal(capped.maxSendsOverride, 1);
  assert.equal(new Fence(capped, 30_000, { maxSends: slackCap }).decide().kind, "readBackRequired");

  // Without the override the same retry may send again: the reason for singleSendCall.
  let uncappedId = "";
  const uncapped = createCall(retryContext(() => uncappedId), connection, operation);
  uncappedId = uncapped.id;
  assert.equal(new Fence(uncapped, 30_000, { maxSends: slackCap }).decide().kind, "readBackThenResendAllowed");
});
