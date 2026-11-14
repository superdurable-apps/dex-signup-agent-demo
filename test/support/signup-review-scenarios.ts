// TEST ONLY: the review scenarios every harness passes, on a private local Dex engine with connector mocks. Reviewer
// replies arrive as Slack Events API envelopes on a Socket Mode stand-in, through the same reply runner and target as
// the application (the Slack connector's MessageTriggerRunner, its durable inbox, and the Dex RPC target).

import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { setTimeout as sleep } from "node:timers/promises";
import test from "node:test";

import { GmailMock, sendMessageBranch } from "@superdurable/connector-google-gmail/mock";
import { GeneratedMock, postThreadReplyUncertain } from "@superdurable/connector-slack/mock";
import { TriggerDeliveryAbortedError, newTriggerContext } from "@superdurable/connectors-sdk";
import type { LoadedProject } from "@superdurable/connectors-sdk/projectconfig";
import { Client, Registry, TimeTravelType, Worker, openBlobCache, type BlobCache } from "@superdurable/dex";
import { readAgentSnapshot } from "@superdurable/dex-agents";
import { PrivateDexEngine, pollUntil } from "@superdurable/dex-agents/testing";

import { ReviewApiClient } from "../../src/review-api.ts";
import { createReviewReplyRunner, createReviewReplyTarget, type ReviewChannel } from "../../src/review-replies.ts";
import { ReviewSessions, reviewSessionId, type ReviewSessionFlow } from "../../src/review-state.ts";
import { type SignupServices } from "../../src/signup-agent.ts";
import { startPendingReviews } from "../../src/signup-watcher.ts";
import { ReviewApiTestStub } from "./review-api-test-stub.ts";
import { FixtureProfileSources, failAfterApproveWord, failAfterSendWord, stayAfterSendWord, multiCallApplicantWord, passiveApplicantWord, rejectedApplicantWord } from "./scripted-signup-agent.ts";
import { RecordingLogger, ReviewThread, SlackEvents, appBotUserId, reviewChannelId, socketModeOptions, socketModeProject } from "./slack-events.ts";

const reviewerId = "UREVIEWER1";
const bystanderId = "UBYSTANDER";
const botUserId = appBotUserId;
const channelId = reviewChannelId;
const channel: ReviewChannel = { channelId, reviewerIds: new Set([reviewerId]) };
const waitOptions = { timeoutMs: 120_000, intervalMs: 200 };
const failure = (operation: string, message: string) => ({ kind: "TRANSPORT" as const, provider: "slack", operation, message });

/** How a harness test builds its session Flow from the shared services. */
export interface SignupReviewScenarioSetup {
  readonly harnessName: string;
  createFlow(services: SignupServices): ReviewSessionFlow | Promise<ReviewSessionFlow>;
  close?(): Promise<void>;
}

/** Registers the scenarios every harness passes. */
export function registerSignupReviewScenarios(setup: SignupReviewScenarioSetup): void {
  let engine: PrivateDexEngine;
  let reviewApi: ReviewApiTestStub;
  let slack: GeneratedMock;
  let gmail: GmailMock;
  let services: SignupServices;
  let client: Client;
  let registry: Registry;
  let worker: Worker;
  let blobCache: BlobCache;
  let cacheDirectory: string;
  let sessionFlow: ReviewSessionFlow;
  let project: LoadedProject;
  const events = new SlackEvents();
  const logger = new RecordingLogger();
  let runner: { stop: AbortController; running: Promise<void> } | undefined;
  let runnerError: unknown;
  let accounts = 0;
  // Sign-ups registered before the tests started are the backlog the watcher must leave alone.
  const reviewSince = new Date(Date.now() - 1_000);
  const watcherParts = () => ({ client, sessionFlow, reviewApi: services.reviewApi, reviewSince });

  test.before(async () => {
    engine = await PrivateDexEngine.start();
    reviewApi = await ReviewApiTestStub.start();
    slack = new GeneratedMock("review-slack");
    gmail = new GmailMock("welcome-gmail");
    services = {
      sessions: new ReviewSessions(),
      reviewApi: new ReviewApiClient({ baseUrl: reviewApi.baseUrl, apiKey: reviewApi.apiKey }),
      profiles: new FixtureProfileSources(),
      slack: slack.connection(),
      gmail: gmail.connection(),
      slackChannelId: channelId,
      harnessLabel: setup.harnessName,
    };
    sessionFlow = await setup.createFlow(services);
    registry = new Registry([sessionFlow]);
    cacheDirectory = mkdtempSync(join(tmpdir(), "signup-agent-test-"));
    blobCache = openBlobCache({ directory: cacheDirectory, maxBytes: 64 * 1024 * 1024 });
    client = new Client(registry, blobCache, { serverAddress: engine.serverAddress });
    services.sessions.connect(client, sessionFlow);
    worker = await startWorker();
    project = await socketModeProject();
    runner = startRunner(services.sessions);
  });

  async function startWorker(): Promise<Worker> {
    const started = new Worker(registry, blobCache, { serverAddress: engine.serverAddress, pull: { stopDrainTimeoutMs: 1_000, maxConcurrentTasks: 16 } });
    await started.start();
    await started.waitPullRegistered({ timeoutMs: 30_000 });
    return started;
  }

  /** Runs the application's reply runner over the Socket Mode stand-in, with `sessions` as its thread index. */
  function startRunner(sessions: ReviewSessions) {
    const stop = new AbortController();
    const target = createReviewReplyTarget({ client, sessionFlow, sessions, channel, logger });
    const running = createReviewReplyRunner(project, channel, target, socketModeOptions(events, logger))
      .run(newTriggerContext(stop.signal))
      .catch((error: unknown) => {
        if (!(error instanceof TriggerDeliveryAbortedError)) {
          runnerError = error;
        }
      });
    return { stop, running };
  }

  async function stopRunner(): Promise<void> {
    runner?.stop.abort();
    await runner?.running;
    runner = undefined;
  }

  test.after(async () => {
    await stopRunner();
    await worker?.close();
    await client?.close();
    blobCache?.close();
    await engine?.stop();
    await reviewApi?.stop();
    await setup.close?.();
    rmSync(cacheDirectory, { recursive: true, force: true });
    slack.assertNoFailures();
    assert.equal(runnerError, undefined);
  });

  const snapshotOf = (sessionId: string) => readAgentSnapshot(client, sessionFlow, sessionId);
  const turnsOf = async (sessionId: string) => (await snapshotOf(sessionId)).state?.turnCount ?? 0;
  const userTexts = async (sessionId: string) => (await snapshotOf(sessionId)).messages.filter((message) => message.role === "user").map((message) => message.text ?? "");
  const threadReplies = (sessionId: string) => slack.postThreadReply().calls().filter((call) => call.flowId === sessionId).map((call) => call.input.text);
  const decisionsOf = (accountId: string) => reviewApi.decisions.filter((decision) => decision.accountId === accountId);
  const sendsOf = (sessionId: string) => gmail.sendMessage().calls().filter((call) => call.flowId === sessionId);
  const sent = (messageId: string) => sendMessageBranch("sent", { sender: "<hello@example.com>", recipients: ["<ada@example.com>"], messageId, threadId: "t-1" });
  const applicant = (accountId: string) => `Ada &lt;Lovelace&gt; admin 'tick' (${reviewApi.accountOf(accountId)!.email})`;
  const drafts = (sessionId: string) => threadReplies(sessionId).filter((text) => text.startsWith("*[draft ")).length;

  /** Waits until the session finished more than `afterTurns` turns and waits for the next message, or has ended. */
  async function waitForIdle(sessionId: string, afterTurns = 0): Promise<void> {
    await pollUntil(
      async () => {
        // finish_review ends the session inside its turn, so an ended session is done too.
        if ((await client.describeFlow(sessionId)).status !== "running") {
          return true;
        }
        const snapshot = await snapshotOf(sessionId);
        return snapshot.state?.status === "waiting_for_user_message" && snapshot.queuedMessageCount === 0 && snapshot.state.turnCount > afterTurns ? true : undefined;
      },
      { description: `${sessionId} waits for a message`, ...waitOptions },
    );
  }

  /** Waits until the session ended, as finish_review ends it. */
  async function waitForEnd(sessionId: string): Promise<void> {
    await pollUntil(async () => ((await client.describeFlow(sessionId)).status === "running" ? undefined : true), { description: `${sessionId} ended`, ...waitOptions });
  }

  /** Waits until the session holds the reply `[slack <ts>]`. */
  async function waitForReply(sessionId: string, ts: string): Promise<void> {
    await pollUntil(async () => ((await userTexts(sessionId)).some((text) => text.startsWith(`[slack ${ts}]`)) ? true : undefined), { description: `the reply ${ts}`, ...waitOptions });
  }

  /** Adds a pending sign-up, starts its review through the watcher, and waits for the review post. */
  async function startReview(emailWord: string, rootSeconds: number) {
    accounts += 1;
    const accountId = `acct${accounts}${Date.now().toString(36)}`;
    const sessionId = reviewSessionId(accountId);
    const thread = new ReviewThread(slack, events, sessionId, `${rootSeconds}.000100`);
    reviewApi.addPendingAccount({ accountId, email: `${emailWord}-${accounts}@example.com`, displayName: "Ada <Lovelace>\nadmin `tick`", profileUrl: "https://github.com/ada-builds" });
    assert.deepEqual(await startPendingReviews(watcherParts()), []);
    const post = await pollUntil(async () => slack.postChannelMessage().calls().find((call) => call.flowId === sessionId)?.input, { description: "the review post", ...waitOptions });
    await waitForIdle(sessionId);
    return { accountId, sessionId, thread, post };
  }

  /** A reviewer reply through Slack, and waits until the session got it and handled it. */
  async function replyAndWait(sessionId: string, thread: ReviewThread, text: string): Promise<string> {
    const turns = await turnsOf(sessionId);
    const { ts } = thread.reply(reviewerId, text);
    await waitForReply(sessionId, ts);
    await waitForIdle(sessionId, turns);
    return ts;
  }

  /** The results the model saw for tool `name`, oldest first. */
  const resultsOf = async (sessionId: string, name: string) => {
    const snapshot = await snapshotOf(sessionId);
    const bare = (toolName: string | null) => toolName?.split("__").at(-1);
    const stored = snapshot.messages.filter((message) => message.role === "tool" && bare(message.toolName) === name).map((message) => message.toolOutput ?? "");
    return stored.length > 0 ? stored : snapshot.toolCalls.filter((record) => bare(record.toolName) === name).map((record) => record.error ?? record.output ?? "");
  };

  test(`${setup.harnessName}: a reviewer revises, approves and sends through Slack replies; the tools confirm each step and the review ends`, async () => {
    const { accountId, sessionId, thread, post } = await startReview("ada", 1710000100);
    assert.equal(post.channelId, channelId);
    assert.match(post.text, /:white_check_mark: Approve/);
    // Applicant and model text is escaped and flattened; the email shows in a code block; the footer is the last line.
    assert.match(post.text, /Ada &lt;Lovelace&gt; admin 'tick'/);
    assert.match(post.text, /&lt;durable-queue&gt; &amp; more/);
    assert.match(post.text, /```\nSubject: Welcome to Dex Cloud\n\nHi Ada,/);
    assert.equal(post.text.split("\n").at(-1), `_Reviewed by ${setup.harnessName} · Dex session \`${sessionId}\`_`);
    // Starting the account's review again joins the same run.
    const { runId } = await client.describeFlow(sessionId);
    assert.deepEqual(await startPendingReviews(watcherParts()), []);
    assert.equal((await client.describeFlow(sessionId)).runId, runId);

    // Never delivered: a bystander's reply, this app's own post and another app's (both carry a bot ID), a bot message
    // subtype, an edit, and a reviewer's message in another channel. The reviewer's feedback after them gives draft 2.
    thread.reply(bystanderId, "approve");
    thread.reply(botUserId, "approve", { bot_id: "BAPP000001" });
    thread.reply("UOTHERBOT01", "approve", { bot_id: "B0OTHERBOT1" });
    thread.reply(reviewerId, "approve", { subtype: "bot_message" });
    thread.reply(reviewerId, "approve", { subtype: "message_changed" });
    thread.reply(reviewerId, "approve", { channel: "COTHER0001" });
    const feedbackTs = await replyAndWait(sessionId, thread, "Mention durable-queue");
    assert.equal(decisionsOf(accountId).length, 0);
    assert.match(threadReplies(sessionId).at(-1)!, /\*\[draft 2\]\*\n```\nSubject: Welcome to Dex Cloud\n\nHi Ada,\n\nWelcome to Dex Cloud. Revised: mention durable-queue/);
    assert.match(threadReplies(sessionId).at(-1)!, /Approve the account first \(`approve`\), then reply `send`, or write what to change\.$/);

    await replyAndWait(sessionId, thread, "approve");
    assert.deepEqual(decisionsOf(accountId), [{ operation: "approve", accountId, requestId: `${sessionId}-approve`, expectedRevision: 0, suppressApprovedMail: true }]);
    assert.equal(reviewApi.accountOf(accountId)?.status, "active");
    assert.equal(threadReplies(sessionId).at(-1), `Approved ${applicant(accountId)} on Dex Cloud. Reply \`send\` to email the welcome message.`);

    // A draft after the approval says the next step is `send`.
    await replyAndWait(sessionId, thread, "Shorter");
    assert.match(threadReplies(sessionId).at(-1)!, /^\*\[draft 3\]\*\n[\s\S]*Reply `send` to send this draft, or write what to change\.$/);
    assert.equal((await services.sessions.review(accountId)).latestDraft?.number, 3);

    gmail.sendMessage().forFlow(sessionId).respond(sent("gmail-1"));
    await replyAndWait(sessionId, thread, "send");
    assert.equal(sendsOf(sessionId).length, 1);
    assert.deepEqual(sendsOf(sessionId)[0]!.input.to, [reviewApi.accountOf(accountId)!.email]);
    assert.match(sendsOf(sessionId)[0]!.input.textBody, /Revised: shorter/);
    assert.ok(threadReplies(sessionId).includes(`Sent the welcome email to ${reviewApi.accountOf(accountId)!.email}.`));
    await waitForEnd(sessionId);

    // Each reviewer reply reached the session once, and nothing else did.
    const relayed = await userTexts(sessionId);
    assert.equal(relayed.filter((text) => text.startsWith(`[slack ${feedbackTs}]`)).length, 1);
    assert.equal(relayed.filter((text) => text.startsWith("[slack ")).length, 4);
    assert.equal(relayed.filter((text) => text.includes(bystanderId) || text.includes(botUserId) || text.includes("UOTHERBOT01")).length, 0);

    // A reply in the thread of the finished review cannot reach its closed session: it is consumed as undeliverable.
    const { eventId } = thread.reply(reviewerId, "one more thing");
    await pollUntil(() => (logger.find("trigger event skipped: undeliverable", "event_id", eventId).length === 1 ? true : undefined), { description: "the late reply consumed", ...waitOptions });
  });

  test(`${setup.harnessName}: the same Slack event delivered twice is handled once`, async () => {
    const { sessionId, thread } = await startReview("twice", 1710000230);
    const turns = await turnsOf(sessionId);
    const { ts, eventId, message } = thread.reply(reviewerId, "Shorter");
    await waitForReply(sessionId, ts);
    await waitForIdle(sessionId, turns);
    assert.equal(drafts(sessionId), 1);
    // Slack sends the event again, as after a lost acknowledgement. Dex does not deduplicate the RPC, so the session
    // may get the message twice; the agent ignores a ts it already saw.
    const before = events.acknowledged.length;
    events.push(message, eventId);
    await pollUntil(() => (events.acknowledged.length > before ? true : undefined), { description: "the repeat acknowledged", ...waitOptions });
    await sleep(1_000);
    await waitForIdle(sessionId);
    assert.equal(drafts(sessionId), 1);
    assert.ok((await userTexts(sessionId)).filter((text) => text.startsWith(`[slack ${ts}]`)).length <= 2);
  });

  test(`${setup.harnessName}: a reviewer's reply in a thread that is no review's is dropped`, async () => {
    const { accountId, sessionId, thread } = await startReview("unrelated", 1710000240);
    const strayThread = "1600000000.000100";
    const strayEvent = events.push({ user: reviewerId, text: "approve", ts: "1600000005.000200", thread_ts: strayThread });
    // The reviewer's next reply in the real thread arrives after the stray one, so the stray one was handled first.
    await replyAndWait(sessionId, thread, "Shorter");
    assert.equal(logger.find("trigger event skipped: not a running review's thread", "event_id", strayEvent).length, 1);
    assert.equal(decisionsOf(accountId).length, 0);
    assert.equal((await userTexts(sessionId)).filter((text) => text.includes("1600000005.000200")).length, 0);
  });

  test(`${setup.harnessName}: an approval the server refuses posts the failure in the thread`, async () => {
    const { accountId, sessionId, thread } = await startReview("nomail", 1710000170);
    reviewApi.refusesSuppressedMail = true;
    try {
      await replyAndWait(sessionId, thread, "approve");
    } finally {
      reviewApi.refusesSuppressedMail = false;
    }
    assert.equal(reviewApi.accountOf(accountId)?.status, "pending_approval");
    assert.equal(threadReplies(sessionId).at(-1), "Approval failed: the review API refused the decision: ReviewAPI approve: suppressApprovedMail is not supported yet.");
    assert.match((await resultsOf(sessionId, "approve_account")).at(-1) ?? "", /approve_account failed: .*suppressApprovedMail is not supported yet/);
  });

  test(`${setup.harnessName}: an approval whose answer was lost is tried again and counts as approved`, async () => {
    const { accountId, sessionId, thread } = await startReview("lost", 1710000200);
    reviewApi.failNextResponse("approve");
    await replyAndWait(sessionId, thread, "approve");
    assert.equal(reviewApi.accountOf(accountId)?.status, "active");
    assert.deepEqual(decisionsOf(accountId).map((decision) => decision.requestId), [`${sessionId}-approve`]);
    assert.match(threadReplies(sessionId).at(-1)!, /^Approved /);
  });

  test(`${setup.harnessName}: an applied approval whose answer was lost, with the ReviewAPI busy since, is an unknown outcome`, async () => {
    const { accountId, sessionId, thread } = await startReview("busyafter", 1710000210);
    // The approval is applied, its answer lost, and every account read after it is busy, so each retry fails.
    reviewApi.failNextResponse("approve", 1_000);
    try {
      await replyAndWait(sessionId, thread, "approve");
    } finally {
      reviewApi.failingGets = 0;
    }
    assert.equal(reviewApi.accountOf(accountId)?.status, "active");
    assert.equal(decisionsOf(accountId).length, 1);
    assert.equal(threadReplies(sessionId).at(-1), "Approval outcome unknown: ReviewAPI get: busy. Check the account in Dex Cloud admin; the review continues.");
    assert.ok(!threadReplies(sessionId).some((text) => text.startsWith("Approval failed")));
    // The model is told the outcome is unknown, not that the approval failed.
    assert.match((await resultsOf(sessionId, "approve_account")).at(-1) ?? "", /outcome (is )?unknown/);
    assert.equal((await client.describeFlow(sessionId)).status, "running");
    // The approval did take effect, so the admins' `send` goes out once the ReviewAPI answers again.
    gmail.sendMessage().forFlow(sessionId).respond(sent("gmail-after-unknown"));
    await replyAndWait(sessionId, thread, "send");
    assert.equal(sendsOf(sessionId).length, 1);
    await waitForEnd(sessionId);
  });

  test(`${setup.harnessName}: an approval repeated after the turn failed is not made or confirmed twice`, async () => {
    const { accountId, sessionId, thread } = await startReview(failAfterApproveWord, 1710000256);
    // The model call after approve_account fails, so that turn fails after its write; the reviewer asks again.
    await replyAndWait(sessionId, thread, "approve");
    assert.equal(decisionsOf(accountId).length, 1);
    await replyAndWait(sessionId, thread, "approve");
    assert.equal(decisionsOf(accountId).length, 1);
    assert.equal(threadReplies(sessionId).filter((text) => text.startsWith("Approved ")).length, 1);
    assert.match((await resultsOf(sessionId, "approve_account")).at(-1) ?? "", /already done earlier in this review/);
  });

  test(`${setup.harnessName}: an approval repeated after an admin rejected the account says so and posts nothing`, async () => {
    const { accountId, sessionId, thread } = await startReview(failAfterApproveWord, 1710000259);
    await replyAndWait(sessionId, thread, "approve");
    assert.equal(decisionsOf(accountId).length, 1);
    // While the approval's turn failed, an admin rejected the account in Dex Cloud admin.
    reviewApi.decideElsewhere(accountId, "rejected");
    await replyAndWait(sessionId, thread, "approve");
    assert.equal(decisionsOf(accountId).length, 1);
    assert.equal(threadReplies(sessionId).filter((text) => /^(Approved |Approval )/.test(text)).length, 1);
    const result = (await resultsOf(sessionId, "approve_account")).at(-1) ?? "";
    assert.match(result, /"status":"changed"/);
    assert.match(result, /now rejected in Dex Cloud/);
  });

  test(`${setup.harnessName}: a send repeated after the turn failed sends one email`, async () => {
    const { sessionId, thread } = await startReview(failAfterSendWord, 1710000257);
    await replyAndWait(sessionId, thread, "approve");
    gmail.sendMessage().forFlow(sessionId).default(sent("gmail-once"));
    await replyAndWait(sessionId, thread, "send");
    assert.equal(sendsOf(sessionId).length, 1);
    await replyAndWait(sessionId, thread, "send");
    assert.equal(sendsOf(sessionId).length, 1);
    assert.match((await resultsOf(sessionId, "send_welcome_email")).at(-1) ?? "", /refused: the welcome email for draft 1 was already sent in this review/);
    assert.equal(threadReplies(sessionId).filter((text) => text.startsWith("Sent the welcome email")).length, 1);
  });

  test(`${setup.harnessName}: time travel of the session to before the send sends it again`, async () => {
    const { sessionId, thread } = await startReview(stayAfterSendWord, 1710000258);
    await replyAndWait(sessionId, thread, "approve");
    gmail.sendMessage().forFlow(sessionId).default(sent("gmail-again"));
    await replyAndWait(sessionId, thread, "send");
    assert.equal(sendsOf(sessionId).length, 1);
    // The latest single-attempt tool Step is the send; re-running it finds the session's records rewound.
    await client.timeTravel(sessionId, { type: TimeTravelType.STEP_TYPE, stepType: "CallSingleAttemptTool", reason: "test: send again" });
    await pollUntil(() => (sendsOf(sessionId).length === 2 ? true : undefined), { description: "the send after time travel", ...waitOptions });
  });

  test(`${setup.harnessName}: a confirmation that fails to post leaves the approval as it is`, async () => {
    const { accountId, sessionId, thread } = await startReview("lostpost", 1710000250);
    slack
      .postThreadReply()
      .forFlow(sessionId)
      .respond(postThreadReplyUncertain({ message: { channelId, timestamp: "", threadTimestamp: thread.rootTs, userId: botUserId, text: "" } }, failure("postThreadReply", "lost")));
    await replyAndWait(sessionId, thread, "approve");
    assert.equal(reviewApi.accountOf(accountId)?.status, "active");
    assert.match((await resultsOf(sessionId, "approve_account")).at(-1) ?? "", /"status":"approved"/);
  });

  test(`${setup.harnessName}: a rejected sign-up is rejected with the reason in the thread and gets no email`, async () => {
    const { accountId, sessionId, thread, post } = await startReview(rejectedApplicantWord, 1710000600);
    assert.match(post.text, /:x: Reject/);
    await replyAndWait(sessionId, thread, "reject Looks like <spam>");
    assert.deepEqual(decisionsOf(accountId).map((decision) => decision.operation), ["reject"]);
    assert.equal(reviewApi.accountOf(accountId)?.status, "rejected");
    assert.ok(threadReplies(sessionId).includes(`Rejected ${applicant(accountId)}: Looks like &lt;spam&gt;.`));
    assert.equal(sendsOf(sessionId).length, 0);
    await waitForEnd(sessionId);
  });

  test(`${setup.harnessName}: a rejection the server refuses posts the failure in the thread`, async () => {
    const { accountId, sessionId, thread } = await startReview("lockedreject", 1710000650);
    reviewApi.refuseNext("reject");
    await replyAndWait(sessionId, thread, "reject looks like spam");
    assert.equal(reviewApi.accountOf(accountId)?.status, "pending_approval");
    assert.equal(threadReplies(sessionId).at(-1), "Rejection failed: the review API refused the decision: ReviewAPI reject: the account is locked.");
  });

  test(`${setup.harnessName}: a send that fails says so in the thread, and the review ends`, async () => {
    const { sessionId, thread } = await startReview("unconfirmed", 1710000500);
    await replyAndWait(sessionId, thread, "approve");
    gmail
      .sendMessage()
      .forFlow(sessionId)
      .respond(sendMessageBranch("defect", { sender: "", recipients: [], messageId: "", threadId: "" }, { kind: "TRANSPORT", provider: "google", operation: "sendMessage", message: "connection reset" }));
    await replyAndWait(sessionId, thread, "send");
    assert.equal(sendsOf(sessionId).length, 1);
    assert.ok(threadReplies(sessionId).includes("Sending the welcome email failed: connection reset. Check Sent Mail; if nothing went out, send it manually."));
    await waitForEnd(sessionId);
  });

  test(`${setup.harnessName}: a send Gmail rejects is a definite failure: nothing was sent`, async () => {
    const { sessionId, thread } = await startReview("gmailoff", 1710000520);
    await replyAndWait(sessionId, thread, "approve");
    gmail
      .sendMessage()
      .forFlow(sessionId)
      .respond(sendMessageBranch("providerRejected", { sender: "", recipients: [], messageId: "", threadId: "" }, { kind: "AUTHORIZATION", provider: "google", operation: "sendMessage", message: "Gmail rejected the message" }));
    await replyAndWait(sessionId, thread, "send");
    assert.equal(sendsOf(sessionId).length, 1);
    assert.ok(threadReplies(sessionId).includes("Sending the welcome email failed: Gmail rejected the message (AUTHORIZATION). Nothing was sent."));
    assert.ok(!threadReplies(sessionId).some((text) => /Check Sent Mail/.test(text)));
    // Recorded as failed with the reason, not as an unknown outcome.
    assert.match((await resultsOf(sessionId, "send_welcome_email")).at(-1) ?? "", /send_welcome_email failed and nothing was sent: Gmail rejected the message \(AUTHORIZATION\)/);
    await waitForEnd(sessionId);
  });

  test(`${setup.harnessName}: a draft the same as the latest one is refused`, async () => {
    const { sessionId, thread } = await startReview("samedraft", 1710000540);
    await replyAndWait(sessionId, thread, "Shorter");
    assert.equal(drafts(sessionId), 1);
    // The same feedback again makes the scripted model write the same draft.
    await replyAndWait(sessionId, thread, "Shorter");
    assert.equal(drafts(sessionId), 1);
    assert.match((await resultsOf(sessionId, "post_draft")).at(-1) ?? "", /refused: this draft is the same as draft 2/);
  });

  test(`${setup.harnessName}: a send before the approval is refused and sends nothing`, async () => {
    const { sessionId, thread } = await startReview("early", 1710000550);
    await replyAndWait(sessionId, thread, "send");
    assert.equal(sendsOf(sessionId).length, 0);
    assert.match((await resultsOf(sessionId, "send_welcome_email")).at(-1) ?? "", /refused: the account is pending_approval, not approved/);
  });

  test(`${setup.harnessName}: several tool calls in one model response keep their order`, async () => {
    const { sessionId, thread } = await startReview(multiCallApplicantWord, 1710000370);
    await replyAndWait(sessionId, thread, "Shorter");
    // pi runs both calls in order; the Claude Code CLI runs one deferred call per segment and may drop the second.
    const firstLines = threadReplies(sessionId).map((text) => text.split("\n")[0]);
    assert.equal(firstLines.filter((line) => line === "*[draft 2]*").length, 1);
    assert.ok(!firstLines.includes("Here is the new draft.") || firstLines.indexOf("Here is the new draft.") > firstLines.indexOf("*[draft 2]*"));
    await replyAndWait(sessionId, thread, "approve");
    gmail.sendMessage().forFlow(sessionId).respond(sent("gmail-multi"));
    await replyAndWait(sessionId, thread, "send");
    assert.equal(sendsOf(sessionId).length, 1);
    // The draft posted beside another call is the one sent.
    assert.match(sendsOf(sessionId)[0]!.input.textBody, /Revised: shorter/);
  });

  test(`${setup.harnessName}: sign-ups registered before SIGNUP_REVIEW_SINCE are left alone`, async () => {
    const accountId = `acctold${Date.now().toString(36)}`;
    reviewApi.addPendingAccount({ accountId, email: "old@example.com", registeredAt: "2020-01-01T00:00:00Z" });
    assert.deepEqual(await startPendingReviews(watcherParts()), []);
    await assert.rejects(client.describeFlow(reviewSessionId(accountId)));
  });

  // Last: it replaces the runner and its index, as a restarted process would have them.
  test(`${setup.harnessName}: after a restart, a reply that arrived while the Worker was down reaches its review once`, async () => {
    const { sessionId, thread } = await startReview(passiveApplicantWord, 1710000900);
    const firstTs = await replyAndWait(sessionId, thread, "first note");
    await stopRunner();
    await worker.close();
    // A new process: its runner's thread index is empty, and the Worker is still down when the reply arrives.
    const restarted = new ReviewSessions();
    restarted.connect(client, sessionFlow);
    runner = startRunner(restarted);
    const before = events.acknowledged.length;
    const { ts: secondTs, eventId } = thread.reply(reviewerId, "second note");
    // Acknowledged once the durable inbox recorded it; its delivery waits for the Worker.
    await pollUntil(() => (events.acknowledged.length > before ? true : undefined), { description: "the reply received while the Worker is down", ...waitOptions });
    await sleep(2_000);
    worker = await startWorker();
    await waitForReply(sessionId, secondTs);
    assert.equal(logger.find("trigger event skipped: not a running review's thread", "event_id", eventId).length, 0);
    const relayed = await userTexts(sessionId);
    for (const ts of [firstTs, secondTs]) {
      assert.equal(relayed.filter((text) => text.startsWith(`[slack ${ts}]`)).length, 1);
    }
  });
}
