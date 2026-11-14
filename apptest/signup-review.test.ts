// The app tests of make test-app: the hosted pi session Flow on its own engine per test, with the watcher and the
// Slack reply Trigger running as the application runs them. A scripted pi model plays the agent, the Slack and Gmail
// connector mocks stand in for the tools' calls, a stub stands in for the ReviewAPI, and reviewer replies arrive as
// Slack events on a Socket Mode stand-in. The scenario suite of both harnesses is `npm test` (test/).

import { strict as assert } from "node:assert";
import { after } from "node:test";

import { GmailMock, sendMessageBranch } from "@superdurable/connector-google-gmail/mock";
import { GeneratedMock } from "@superdurable/connector-slack/mock";
import { ScriptedModelClient } from "@superdurable/dex-agents/testing";

import { reviewSessionId } from "../src/review-state.ts";
import { ReviewApiTestStub } from "../test/support/review-api-test-stub.ts";
import { FixtureProfileSources, decideNextStep } from "../test/support/scripted-signup-agent.ts";
import { RecordingLogger, ReviewThread, SlackEvents, appBotUserId, reviewChannelId, socketModeOptions, socketModeProject } from "../test/support/slack-events.ts";
import { apptest, type AppEnv } from "./apptest.ts";

const reviewerId = "UREVIEWER1";
const reviewApi = await ReviewApiTestStub.start();
after(() => reviewApi.stop());
const slack = new GeneratedMock("review-slack");
const gmail = new GmailMock("welcome-gmail");
const events = new SlackEvents();
const logger = new RecordingLogger();

// The scripted model follows the agent's procedure from the session's messages and tool results only.
const model = new ScriptedModelClient((request) => {
  const seen = ScriptedModelClient.toolResults(request).map((result) => ({ name: result.toolName, output: result.output }));
  const step = decideNextStep(ScriptedModelClient.userTexts(request), seen);
  if (step.kind === "fail") {
    throw new Error("scripted model failure");
  }
  return step.kind === "answer" ? { text: step.text } : { toolCalls: step.calls.map((call) => ({ toolName: call.name, arguments: call.arguments })) };
});

const options = {
  environment: {
    REVIEW_API_URL: reviewApi.baseUrl,
    REVIEW_API_KEY: reviewApi.apiKey,
    SIGNUP_REVIEW_SINCE: new Date(Date.now() - 60_000).toISOString(),
    SIGNUP_WATCH_INTERVAL_SECONDS: "1",
    SLACK_REVIEW_CHANNEL: reviewChannelId,
    SIGNUP_REVIEWER_SLACK_IDS: reviewerId,
    SIGNUP_AGENT_HARNESS: "pi",
  },
  project: await socketModeProject(),
  slack: slack.connection(),
  gmail: gmail.connection(),
  profiles: new FixtureProfileSources(),
  model,
  slackOptions: socketModeOptions(events, logger),
  triggerLogger: logger,
};

let accounts = 0;
const threadReplies = (sessionId: string) => slack.postThreadReply().calls().filter((call) => call.flowId === sessionId).map((call) => call.input.text);
const decisionsOf = (accountId: string) => reviewApi.decisions.filter((decision) => decision.accountId === accountId);

/** A pending sign-up, which the watcher starts reviewing; resolves once the review is posted. */
async function startReview(env: AppEnv, rootSeconds: number) {
  accounts += 1;
  const accountId = `app${accounts}${Date.now().toString(36)}`;
  const sessionId = reviewSessionId(accountId);
  const thread = new ReviewThread(slack, events, sessionId, `${rootSeconds}.000100`);
  reviewApi.addPendingAccount({ accountId, email: `ada-${accountId}@example.com`, displayName: "Ada Lovelace", profileUrl: "https://github.com/ada-builds" });
  await env.eventually(30_000, async () => slack.postChannelMessage().calls().some((call) => call.flowId === sessionId));
  return { accountId, sessionId, thread };
}

apptest(
  "TestReviewIsApprovedAndSentFromSlackReplies",
  async (env) => {
    const { accountId, sessionId, thread } = await startReview(env, 1720000100);
    thread.reply(reviewerId, "Shorter");
    await env.eventually(30_000, async () => threadReplies(sessionId).some((text) => text.startsWith("*[draft 2]*")));
    thread.reply(reviewerId, "approve");
    await env.eventually(30_000, async () => threadReplies(sessionId).some((text) => text.startsWith("Approved ")));
    assert.equal(reviewApi.accountOf(accountId)?.status, "active");
    gmail.sendMessage().forFlow(sessionId).respond(sendMessageBranch("sent", { sender: "<hello@example.com>", recipients: ["<ada@example.com>"], messageId: "gmail-app-1", threadId: "t-1" }));
    thread.reply(reviewerId, "send");
    // finish_review ends the session after the send.
    await env.eventually(30_000, async () => (await env.dex.describeFlow(sessionId)).status !== "running");
    const sends = gmail.sendMessage().calls().filter((call) => call.flowId === sessionId);
    assert.equal(sends.length, 1);
    assert.match(sends[0]!.input.textBody, /Revised: shorter/);
    assert.ok(threadReplies(sessionId).includes(`Sent the welcome email to ${reviewApi.accountOf(accountId)!.email}.`));
  },
  options,
);

apptest(
  "TestOnlyReviewerRepliesInReviewThreadsAreDelivered",
  async (env) => {
    const { accountId, sessionId, thread } = await startReview(env, 1720000200);
    thread.reply("UBYSTANDER", "approve");
    thread.reply(appBotUserId, "approve", { bot_id: "BAPP000001" });
    const stray = events.push({ user: reviewerId, text: "approve", ts: "1600000005.000200", thread_ts: "1600000000.000100" });
    thread.reply(reviewerId, "Shorter");
    // Replies are delivered in order, so the ones before the reviewer's feedback were handled first.
    await env.eventually(30_000, async () => threadReplies(sessionId).some((text) => text.startsWith("*[draft 2]*")));
    assert.equal(decisionsOf(accountId).length, 0);
    assert.equal(logger.find("trigger event skipped: not a running review's thread", "event_id", stray).length, 1);
  },
  options,
);

apptest(
  "TestHealthAnswers",
  async (env) => {
    assert.deepEqual(await env.request("GET", "/api/health"), { status: 200, body: { status: "ok" } });
  },
  options,
);
