// The onboarding wizard's app tests: the hosted pi review session and the Vercel AI SDK onboarding session on one
// engine per test, with both watchers and the Slack reply Trigger running as the application runs them. Scripted
// models play both agents, the connector mocks stand in for Slack and Gmail, stubs for the ReviewAPI and the usage
// operations, and a test clock moves the onboarding days forward.

import { strict as assert } from "node:assert";
import { after } from "node:test";

import { GmailMock, sendMessageBranch } from "@superdurable/connector-google-gmail/mock";
import { GeneratedMock } from "@superdurable/connector-slack/mock";
import { ScriptedModelClient } from "@superdurable/dex-agents/testing";

import { onboardingSessionId } from "../src/onboarding-state.ts";
import { reviewSessionId } from "../src/review-state.ts";
import { ReviewApiTestStub } from "../test/support/review-api-test-stub.ts";
import { scriptedOnboardingModel } from "../test/support/scripted-onboarding-model.ts";
import { FixtureProfileSources, decideNextStep } from "../test/support/scripted-signup-agent.ts";
import { RecordingLogger, ReviewThread, SlackEvents, socketModeOptions, socketModeProject } from "../test/support/slack-events.ts";
import { UsageStandIn, type StandInAccount } from "../tools/usage-standin.ts";
import { apptest, type AppEnv } from "./apptest.ts";

const reviewerId = "UREVIEWER1";
const dayMs = 24 * 60 * 60 * 1_000;
/** The test clock: real time plus `skew`, shared by the onboarding tools, the watcher and the usage stand-in. */
let skew = 0;
const now = () => Date.now() + skew;

const reviewApi = await ReviewApiTestStub.start();
const usage: Record<string, StandInAccount> = {};
const usageApi = await UsageStandIn.start({ read: () => usage, now });
after(() => Promise.all([reviewApi.stop(), usageApi.stop()]));
const slack = new GeneratedMock("review-slack");
const gmail = new GmailMock("welcome-gmail");
const events = new SlackEvents();
const logger = new RecordingLogger();

const reviewModel = new ScriptedModelClient((request) => {
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
    SLACK_REVIEW_CHANNEL: "CREVIEWS01",
    SIGNUP_REVIEWER_SLACK_IDS: reviewerId,
    SIGNUP_AGENT_HARNESS: "pi",
    ANTHROPIC_API_KEY: "sk-ant-test-not-a-real-key",
    ONBOARDING_ENABLED: "true",
    ONBOARDING_SINCE: new Date(Date.now() - 60_000).toISOString(),
    ONBOARDING_USAGE_API_URL: usageApi.baseUrl,
    ONBOARDING_USAGE_API_KEY: usageApi.apiKey,
    ONBOARDING_WATCH_INTERVAL_SECONDS: "1",
  },
  project: await socketModeProject(),
  slack: slack.connection(),
  gmail: gmail.connection(),
  profiles: new FixtureProfileSources(),
  model: reviewModel,
  onboardingModel: scriptedOnboardingModel(),
  now,
  slackOptions: socketModeOptions(events, logger),
  triggerLogger: logger,
};

const posts = (sessionId: string) => slack.postThreadReply().calls().filter((call) => call.flowId === sessionId).map((call) => call.input.text);
const rootPosts = (sessionId: string) => slack.postChannelMessage().calls().filter((call) => call.flowId === sessionId).map((call) => call.input.text);

/** Waits until the onboarding finished `turns` turns and waits for its next message, so the clock moves only between turns. */
async function idle(env: AppEnv, accountId: string, turns: number): Promise<void> {
  await env.eventually(30_000, async () => {
    const state = (await env.application.onboarding!.services.sessions.snapshot(accountId)).state;
    return state !== null && state.activeTurnId === null && state.turnCount >= turns;
  });
}

/** A sign-up the review approves, which hands the account over to its onboarding; resolves once the report is posted. */
async function approvedAccount(env: AppEnv, reviewRoot: number, onboardingRoot: number) {
  const accountId = `onb${reviewRoot}${Date.now().toString(36)}`;
  const review = new ReviewThread(slack, events, reviewSessionId(accountId), `${reviewRoot}.000100`);
  const onboarding = new ReviewThread(slack, events, onboardingSessionId(accountId), `${onboardingRoot}.000100`);
  reviewApi.addPendingAccount({ accountId, email: `ada-${accountId}@example.com`, displayName: "Ada Lovelace", profileUrl: "https://github.com/ada-builds", purpose: "A support agent for our help desk" });
  usage[accountId] = { approvedAt: new Date(now()).toISOString() };
  await env.eventually(30_000, async () => rootPosts(reviewSessionId(accountId)).length > 0);
  review.reply(reviewerId, "approve");
  await env.eventually(30_000, async () => rootPosts(onboardingSessionId(accountId)).length > 0);
  await idle(env, accountId, 1);
  return { accountId, sessionId: onboardingSessionId(accountId), onboarding };
}

apptest(
  "TestApprovalStartsOnboardingThatNudgesAStalledAccountAndFinishes",
  async (env) => {
    const { accountId, sessionId, onboarding } = await approvedAccount(env, 1730000100, 1730000500);
    // The first report: the metrics come from the tool, the headline and insights from the model.
    const report = rootPosts(sessionId)[0]!;
    assert.match(report, /^:rocket: \*Onboarding:\* Ada Lovelace/);
    assert.match(report, /\*Stage:\* 1\/6 approved/);
    assert.match(report, /A support agent for our help desk/);
    // The account signs in once and then goes quiet; three days later its check-in is due and finds it stalled.
    usage[accountId] = { ...usage[accountId], lastSignInAt: new Date(now()).toISOString(), lastActivityAt: new Date(now()).toISOString(), activeDays7d: 1, activeDays28d: 1, milestones: { firstSignInAt: new Date(now()).toISOString() } };
    skew += 6 * dayMs;
    await env.eventually(30_000, async () => posts(sessionId).some((text) => text.startsWith("*[nudge 1]*")));
    assert.ok(posts(sessionId).some((text) => /^\*Check-in, day 6:\*/.test(text) && /\*Signal:\* stalled/.test(text)));
    // An admin approves the nudge in the onboarding thread; it goes to the customer once.
    gmail.sendMessage().forFlow(sessionId).respond(sendMessageBranch("sent", { sender: "<hello@example.com>", recipients: ["<ada@example.com>"], messageId: "gmail-onb-1", threadId: "t-1" }));
    onboarding.reply(reviewerId, "send");
    await env.eventually(30_000, async () => posts(sessionId).includes("Sent the nudge."));
    await idle(env, accountId, 3);
    const sends = gmail.sendMessage().calls().filter((call) => call.flowId === sessionId);
    assert.equal(sends.length, 1);
    assert.deepEqual(sends[0]!.input.to, [`ada-${accountId}@example.com`]);
    // A second `send` is refused by the tool: the nudge already went out.
    onboarding.reply(reviewerId, "send");
    await env.eventually(30_000, async () => posts(sessionId).includes("The nudge was not sent."));
    await idle(env, accountId, 4);
    assert.equal(gmail.sendMessage().calls().filter((call) => call.flowId === sessionId).length, 1);
    // Past the onboarding window, the last check-in wraps up and the session completes.
    skew += 30 * dayMs;
    await env.eventually(30_000, async () => (await env.dex.describeFlow(sessionId)).status !== "running");
    assert.equal((await env.dex.describeFlow(sessionId)).status, "completed");
    assert.ok(posts(sessionId).includes("Onboarding closed: the window ended."));
  },
  options,
);
