// The released Slack and Gmail connector operations with the real Connector SDK fence, over a fake provider transport
// and a fake tool Step: Dex Agents' ConnectorStepContext over one text checkpoint that the next attempt reads, as the
// session's tool Step keeps it. Each `attempt` is one attempt of that Step.

import assert from "node:assert/strict";
import test from "node:test";

import { GmailClient } from "@superdurable/connector-google-gmail";
import { SlackClient } from "@superdurable/connector-slack";
import { SecretString, StaticCredentialProvider, createCall, type Call, type ConnectionRef, type Mutation, type MutationAttempt } from "@superdurable/connectors-sdk";
import type { ProviderRequest, ProviderResponse } from "@superdurable/connectors-sdk/providerhttp";
import { ConnectorStepContext } from "@superdurable/dex-agents/connectors";
import type { AsyncContext } from "@superdurable/dex";

import { confirmationStateOf } from "../src/signup-agent.ts";

const slackConnection: ConnectionRef = { provider: "slack", name: "review-slack" };
const gmailConnection: ConnectionRef = { provider: "gmail", name: "welcome-gmail" };
const replyInput = { channelId: "C0REVIEW", threadTimestamp: "1710000100.000100", text: "Approved." };
const emailInput = { to: ["ada@example.com"], subject: "Welcome to Dex Cloud", textBody: "Hi Ada" };

/** One tool Step: its attempts share one text checkpoint, which an attempt reads as the one saved before it. */
class FakeToolStep {
  private attempts = 0;
  private checkpoint: string | undefined;

  /** The context of the Step's next attempt. */
  public next(): ConnectorStepContext {
    this.attempts += 1;
    const context = {
      flowId: "signup-review-acct1",
      runId: "run-1",
      flowStartedAt: new Date(0),
      firstAttemptAt: new Date(0),
      stepExecutionId: "step-1",
      attempt: this.attempts,
      deadline: new Date(Date.now() + 30_000),
      cancellationSignal: new AbortController().signal,
    } as unknown as AsyncContext;
    return new ConnectorStepContext(context, {
      previousCheckpoint: this.checkpoint,
      heartbeat: async () => undefined,
      saveCheckpoint: async (text: string) => {
        this.checkpoint = text;
      },
    });
  }

  /** The call ID the checkpoint belongs to, or `undefined`. */
  public checkpointOwner(): string | undefined {
    return this.checkpoint === undefined ? undefined : (JSON.parse(this.checkpoint) as { callId: string }).callId;
  }
}

/** Runs `operation` once in `context`, with the call identity and idempotency key the session's tool Step gives it. */
async function attempt<Input, Output>(operation: Mutation<Input, Output>, context: ConnectorStepContext, connection: ConnectionRef, input: Input): Promise<MutationAttempt<Output>> {
  const call: Call = createCall(context as unknown as AsyncContext, connection, operation.definition().operation);
  call.idempotencyKey = operation.idempotencyKey(call.id, input);
  return operation.invoke(call, input);
}

const json = (status: number, body: unknown): ProviderResponse => ({ status, headers: {}, body: new TextEncoder().encode(JSON.stringify(body)) });
/** A transport that records each request and answers it with `answer`. */
function transport(answer: (request: ProviderRequest) => ProviderResponse | Promise<ProviderResponse>) {
  const requests: ProviderRequest[] = [];
  return { requests, send: async (request: ProviderRequest) => (requests.push(request), answer(request)) };
}
const bodyOf = (request: ProviderRequest) => JSON.parse(new TextDecoder().decode(request.body)) as Record<string, unknown>;

function slackClient(send: (request: ProviderRequest) => Promise<ProviderResponse>) {
  const credentials = new StaticCredentialProvider([[slackConnection, { botToken: new SecretString("xoxb-test"), userToken: new SecretString("xoxp-test") }]]);
  return new SlackClient({}, credentials as never, { send });
}
function gmailClient(send: (request: ProviderRequest) => Promise<ProviderResponse>) {
  const credentials = new StaticCredentialProvider([[gmailConnection, { accessToken: new SecretString("ya29.test"), primaryEmail: "hello@example.com" }]]);
  return new GmailClient({}, credentials as never, { transport: send });
}
const isPost = (request: ProviderRequest) => request.url.endsWith("/chat.postMessage");
const isGmailSend = (request: ProviderRequest) => request.method === "POST" && request.url.includes("/messages/send");
const isSentMailSearch = (request: ProviderRequest) => request.method === "GET" && decodeURIComponent(request.url).includes("rfc822msgid:");

test("Slack postThreadReply sends at most 3 times with one client_msg_id when each outcome is unknown", async () => {
  const provider = transport(() => {
    throw new Error("connection reset after the request was written");
  });
  const reply = slackClient(provider.send).postThreadReply();
  const step = new FakeToolStep();
  const kinds: string[] = [];
  for (let index = 0; index < 4; index++) {
    kinds.push((await attempt(reply, step.next(), slackConnection, replyInput)).kind);
  }
  // Two retries resend; the third send selects uncertain at the cap; a fourth attempt cannot identify a post to read.
  assert.deepEqual(kinds, ["retry", "retry", "uncertain", "uncertain"]);
  const posts = provider.requests.filter(isPost);
  assert.equal(posts.length, 3);
  assert.equal(new Set(posts.map((request) => bodyOf(request).client_msg_id)).size, 1);
});

test("Slack reads back a post whose ts was checkpointed and never resends it", async () => {
  let isReplyReadable = false;
  const provider = transport((request) => {
    if (isPost(request)) {
      // Accepted, but the answer lacks the channel: the connector checkpoints the ts and reads the post back.
      return json(200, { ok: true, ts: "1710000100.000900" });
    }
    return isReplyReadable
      ? json(200, { ok: true, messages: [{ ts: "1710000100.000900", thread_ts: replyInput.threadTimestamp, text: replyInput.text, user: "UBOT" }] })
      : json(500, { ok: false, error: "internal_error" });
  });
  const reply = slackClient(provider.send).postThreadReply();
  const step = new FakeToolStep();
  assert.equal((await attempt(reply, step.next(), slackConnection, replyInput)).kind, "retry");
  isReplyReadable = true;
  const second = await attempt(reply, step.next(), slackConnection, replyInput);
  assert.equal(second.kind === "branch" ? second.branch : second.kind, "sent");
  assert.equal(provider.requests.filter(isPost).length, 1);
  assert.equal(confirmationStateOf(second), "posted");
});

test("a Slack post Slack rejects is a definite failure: never resent, and a note that was not posted", async () => {
  const provider = transport(() => json(200, { ok: false, error: "not_in_channel" }));
  const reply = slackClient(provider.send).postThreadReply();
  const result = await attempt(reply, new FakeToolStep().next(), slackConnection, replyInput);
  assert.equal(result.kind === "branch" ? result.branch : result.kind, "providerRejected");
  assert.equal(provider.requests.filter(isPost).length, 1);
  assert.equal(confirmationStateOf(result), "notPosted");
});

test("a Slack note whose answer was lost is unknown, not 'not posted'", async () => {
  const provider = transport(() => json(500, { ok: false, error: "internal_error" }));
  const reply = slackClient(provider.send).postThreadReply();
  const result = await attempt(reply, new FakeToolStep().next(), slackConnection, replyInput);
  assert.equal(result.kind, "retry");
  assert.equal(confirmationStateOf(result), "unknown");
});

test("Gmail reads back Sent Mail by the shared Message-ID before it would resend", async () => {
  let isInSentMail = false;
  const provider = transport((request) => {
    if (isGmailSend(request)) {
      return json(500, { error: { code: 500 } });
    }
    if (isSentMailSearch(request)) {
      return json(200, isInSentMail ? { messages: [{ id: "m-1", threadId: "t-1" }] } : {});
    }
    return json(404, {});
  });
  const send = gmailClient(provider.send).sendMessage();
  const step = new FakeToolStep();
  assert.equal((await attempt(send, step.next(), gmailConnection, emailInput)).kind, "retry");
  // The 500 lost the answer, but the message reached Sent Mail: the next attempt finds it there.
  isInSentMail = true;
  const second = await attempt(send, step.next(), gmailConnection, emailInput);
  assert.equal(second.kind === "branch" ? second.branch : second.kind, "sent");
  assert.equal(second.kind === "branch" ? (second.value as { messageId: string }).messageId : "", "m-1");
  assert.equal(provider.requests.filter(isGmailSend).length, 1);
  const searched = new Set(provider.requests.filter(isSentMailSearch).map((request) => /rfc822msgid:(\S+)/.exec(decodeURIComponent(request.url))![1]));
  assert.equal(searched.size, 1, "every search looks for the one Message-ID of the call");
});

test("Gmail sends at most 3 times, searching Sent Mail before each resend", async () => {
  const provider = transport((request) => (isGmailSend(request) ? json(500, { error: { code: 500 } }) : isSentMailSearch(request) ? json(200, {}) : json(404, {})));
  const send = gmailClient(provider.send).sendMessage();
  const step = new FakeToolStep();
  const kinds: string[] = [];
  for (let index = 0; index < 4; index++) {
    kinds.push((await attempt(send, step.next(), gmailConnection, emailInput)).kind);
  }
  // At the cap Gmail only searches again; Dex records outcome_unknown when the Step's attempts run out.
  assert.deepEqual(kinds, ["retry", "retry", "retry", "retry"]);
  assert.equal(provider.requests.filter(isGmailSend).length, 3);
  const raws = provider.requests.filter(isGmailSend).map((request) => Buffer.from(String(bodyOf(request).raw), "base64url").toString("utf8"));
  assert.equal(new Set(raws.map((raw) => /^Message-ID: (.*)$/m.exec(raw)?.[1])).size, 1, "every resend carries the same Message-ID");
});

test("a send Gmail rejects is a definite failure and is never resent", async () => {
  const provider = transport((request) => (isGmailSend(request) ? json(400, { error: { code: 400, message: "Invalid To header" } }) : json(404, {})));
  const send = gmailClient(provider.send).sendMessage();
  const result = await attempt(send, new FakeToolStep().next(), gmailConnection, emailInput);
  assert.equal(result.kind === "branch" ? result.branch : result.kind, "providerRejected");
  assert.equal(provider.requests.filter(isGmailSend).length, 1);
});

test("a second fenced write in one tool Step takes over the Step's checkpoint, so the app makes one per Step", async () => {
  // dex-connectors-library#870: only one fenced connector write per Step. This is why send_welcome_email posts no note.
  const gmail = transport((request) => (isGmailSend(request) ? json(200, { id: "m-1", threadId: "t-1" }) : json(404, {})));
  const slack = transport(() => json(200, { ok: true, channel: replyInput.channelId, ts: "1710000100.000900", message: { ts: "1710000100.000900", text: replyInput.text } }));
  const send = gmailClient(gmail.send).sendMessage();
  const reply = slackClient(slack.send).postThreadReply();
  const step = new FakeToolStep();
  const first = step.next();
  const sent = await attempt(send, first, gmailConnection, emailInput);
  assert.equal(sent.kind === "branch" ? sent.branch : sent.kind, "sent");
  // In the same first attempt the second write still goes out with SDK 0.11.0: no checkpoint was read before it.
  const posted = await attempt(reply, first, slackConnection, replyInput);
  assert.equal(posted.kind === "branch" ? posted.branch : posted.kind, "sent");
  // But its marker replaced the email's in the Step's one checkpoint. Run again, as after a crash, the email's fence
  // finds a foreign checkpoint and fails closed: no read-back and no resend for the email.
  const second = step.next();
  assert.notEqual(step.checkpointOwner(), undefined);
  const again = await attempt(send, second, gmailConnection, emailInput);
  assert.equal(again.kind === "branch" ? again.branch : again.kind, "defect");
  assert.equal(gmail.requests.filter(isGmailSend).length, 1);
});
