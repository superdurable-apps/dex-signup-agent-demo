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
import { GmailMailbox, isGmailSend, isHeaderRead, isSentMailSearch, rawOf } from "./support/gmail-mailbox.ts";

const slackConnection: ConnectionRef = { provider: "slack", name: "review-slack" };
const gmailConnection: ConnectionRef = { provider: "gmail", name: "welcome-gmail" };
const replyInput = { channelId: "C0REVIEW", threadTimestamp: "1710000100.000100", text: "Approved." };
const emailInput = { to: ["ada@example.com"], subject: "Welcome to Dex Cloud", textBody: "Hi Ada" };

/** One tool Step: its attempts share one text checkpoint, which an attempt reads as the one saved before it. */
class FakeToolStep {
  private attempts: number;
  private checkpoint: string | undefined;

  /** A Step whose next attempt follows `attempts` earlier ones and reads `checkpoint`, as on a new Worker. */
  public constructor(attempts = 0, checkpoint?: string) {
    this.attempts = attempts;
    this.checkpoint = checkpoint;
  }

  /** The checkpoint the attempts saved so far. */
  public get savedCheckpoint(): string | undefined {
    return this.checkpoint;
  }

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
/**
 * Every Sent Mail search listed recent candidates to the recipient, never by Message-ID, and Gmail had replaced the
 * Message-ID of each Sent copy, so only X-Dex-Message-ID could match.
 */
function assertReadBackByDexMessageId(mailbox: GmailMailbox) {
  for (const query of mailbox.searches) {
    assert.equal(query, "in:sent newer_than:2d to:ada@example.com");
  }
  for (const send of mailbox.sends) {
    assert.notEqual(send.messageId, "");
    assert.equal(send.dexMessageId, send.messageId, "the send repeats its Message-ID in X-Dex-Message-ID");
    assert.notEqual(send.sentMessageId, send.messageId, "Gmail replaced the Message-ID of the Sent copy");
  }
}

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

test("Gmail reads Sent Mail back by X-Dex-Message-ID before it would resend", async () => {
  const mailbox = new GmailMailbox();
  let late = "";
  const provider = transport(
    mailbox.answering((raw) => {
      late = raw;
      return json(500, { error: { code: 500 } });
    }),
  );
  const send = gmailClient(provider.send).sendMessage();
  const step = new FakeToolStep();
  assert.equal((await attempt(send, step.next(), gmailConnection, emailInput)).kind, "retry");
  // The 500 lost the answer, but the message reached Sent Mail after the attempt's read-back, with Gmail's own
  // Message-ID: the next attempt finds it by its X-Dex-Message-ID.
  mailbox.send(late);
  const second = await attempt(send, step.next(), gmailConnection, emailInput);
  assert.equal(second.kind === "branch" ? second.branch : second.kind, "sent");
  assert.equal(second.kind === "branch" ? (second.value as { messageId: string }).messageId : "", "gmail-1");
  assert.equal(provider.requests.filter(isGmailSend).length, 1);
  assert.ok(provider.requests.filter(isHeaderRead).length > 0, "a candidate's headers were read");
  assertReadBackByDexMessageId(mailbox);
});

test("a Worker killed after Gmail accepted the send: attempt 2 finds it by X-Dex-Message-ID and sends no second email", async () => {
  const mailbox = new GmailMailbox();
  const step = new FakeToolStep();
  let claim: string | undefined;
  const killed = transport((request) => {
    if (!isGmailSend(request)) {
      throw new Error("the Worker is gone");
    }
    // The fence saved its claim before the send; that is the checkpoint Dex keeps for the next attempt.
    claim = step.savedCheckpoint;
    mailbox.send(rawOf(request));
    throw new Error("the Worker was killed after Gmail accepted the send");
  });
  // The attempt's outcome is lost with its Worker.
  await attempt(gmailClient(killed.send).sendMessage(), step.next(), gmailConnection, emailInput).catch(() => undefined);
  assert.match(claim ?? "", /"state":"claimed"/);
  const alive = transport(mailbox.answering((raw) => mailbox.accept(raw)));
  const second = await attempt(gmailClient(alive.send).sendMessage(), new FakeToolStep(1, claim).next(), gmailConnection, emailInput);
  assert.equal(second.kind === "branch" ? second.branch : second.kind, "sent");
  assert.equal(second.kind === "branch" ? (second.value as { messageId: string }).messageId : "", "gmail-1");
  assert.equal(alive.requests.filter(isGmailSend).length, 0, "attempt 2 sent the email again");
  assert.equal(mailbox.sends.length, 1);
  assertReadBackByDexMessageId(mailbox);
});

test("Gmail sends at most 3 times, searching Sent Mail before each resend", async () => {
  const mailbox = new GmailMailbox();
  const provider = transport(mailbox.answering(() => json(500, { error: { code: 500 } })));
  const send = gmailClient(provider.send).sendMessage();
  const step = new FakeToolStep();
  const kinds: string[] = [];
  for (let index = 0; index < 4; index++) {
    kinds.push((await attempt(send, step.next(), gmailConnection, emailInput)).kind);
  }
  // Two retries resend; the third send selects uncertain at the cap, as Slack does; a fourth attempt only searches
  // Sent Mail again. Dex records outcome_unknown.
  assert.deepEqual(kinds, ["retry", "retry", "uncertain", "uncertain"]);
  const sends = provider.requests.filter(isGmailSend);
  assert.equal(sends.length, 3);
  const ids = new Set(sends.map((request) => `${/^Message-ID: (.*)$/m.exec(rawOf(request))?.[1]} ${/^X-Dex-Message-ID: (.*)$/m.exec(rawOf(request))?.[1]}`));
  assert.equal(ids.size, 1, "every resend carries the same Message-ID");
  const [messageId, dexMessageId] = [...ids][0]!.split(" ");
  assert.equal(dexMessageId, messageId, "and repeats it in X-Dex-Message-ID");
  assert.ok(provider.requests.filter(isSentMailSearch).length > 0, "a resend searched Sent Mail first");
  assertReadBackByDexMessageId(mailbox);
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
  const mailbox = new GmailMailbox();
  const gmail = transport(mailbox.answering((raw) => mailbox.accept(raw)));
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
  // finds a foreign checkpoint and fails closed: no read-back and no resend for the email, though the email is in
  // Sent Mail. Gmail 0.5.1 asks for a retry with a LOCAL_DEFECT failure there; 0.4.0 selected defect.
  const second = step.next();
  assert.notEqual(step.checkpointOwner(), undefined);
  const again = await attempt(send, second, gmailConnection, emailInput);
  assert.equal(again.kind, "retry");
  assert.equal(again.failure?.kind, "LOCAL_DEFECT");
  assert.equal(gmail.requests.length, 1, "the email's retry made no request");
  assert.equal(mailbox.sends.length, 1);
});
