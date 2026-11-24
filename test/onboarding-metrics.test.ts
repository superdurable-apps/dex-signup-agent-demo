// The onboarding metrics and their limits, computed in code: stage, signal, nudge budget, the check-in schedule's window.

import { strict as assert } from "node:assert";
import { test } from "node:test";

import { nudgeBudget, defaultOnboardingPolicy, windowEndOf } from "../src/onboarding-agent.ts";
import { assessUsage, needsFollowUp } from "../src/onboarding-metrics.ts";
import { onboardingRecordOf, nudgeSendOf } from "../src/onboarding-state.ts";
import type { ToolRecord } from "../src/review-state.ts";
import { usageOf, type StandInAccount } from "../tools/usage-standin.ts";

const now = Date.parse("2026-11-23T12:00:00Z");
const usage = (account: StandInAccount) => usageOf("acct1", account, now);

test("an account that never signs in is not_started after two days, progressing before", () => {
  assert.equal(assessUsage(usage({ approvedAt: "-1d" })).signal, "progressing");
  const late = assessUsage(usage({ approvedAt: "-2d" }));
  assert.deepEqual([late.stage, late.signal, late.daysSinceApproval, late.nextStage], ["approved", "not_started", 2, "signed_in"]);
});

test("a later milestone sets the stage even without the earlier ones", () => {
  const assessment = assessUsage(usage({ approvedAt: "-3d", lastActivityAt: "-1h", milestones: { firstSignInAt: "-3d", firstPreviewReadyAt: "-1d" } }));
  assert.deepEqual([assessment.stage, assessment.stageNumber, assessment.daysInStage, assessment.signal], ["preview", 5, 1, "progressing"]);
});

test("four quiet days after signing in is stalled; repeated Preview failures without a ready Preview is blocked", () => {
  assert.equal(assessUsage(usage({ approvedAt: "-6d", lastActivityAt: "-4d", milestones: { firstSignInAt: "-5d" } })).signal, "stalled");
  assert.equal(assessUsage(usage({ approvedAt: "-6d", lastActivityAt: "-3d", milestones: { firstSignInAt: "-5d" } })).signal, "progressing");
  const blocked = assessUsage(
    usage({
      approvedAt: "-6d",
      lastActivityAt: "-1h",
      milestones: { firstSignInAt: "-5d", projectCreatedAt: "-4d" },
      counts: { previewFailures7d: 2 },
      projects: [{ name: "helpdesk", createdAt: "-4d", lastActivityAt: "-1h", stage: "building", lastPreviewFailure: { at: "-1h", message: "build failed: missing OPENAI_API_KEY" } }],
    }),
  );
  assert.deepEqual([blocked.signal, blocked.lastPreviewFailure], ["blocked", "build failed: missing OPENAI_API_KEY"]);
});

test("live: idle without activity or Trigger events, graduated after seven active days live", () => {
  const live = { approvedAt: "-20d", milestones: { firstSignInAt: "-19d", firstLiveAt: "-8d" } } satisfies StandInAccount;
  assert.equal(assessUsage(usage({ ...live, lastActivityAt: "-9d" })).signal, "live_idle");
  assert.equal(assessUsage(usage({ ...live, lastActivityAt: "-9d", counts: { triggerEvents7d: 4 } })).signal, "graduated");
  assert.equal(assessUsage(usage({ ...live, lastActivityAt: "-1d", milestones: { ...live.milestones, firstLiveAt: "-2d" } })).signal, "progressing");
  assert.deepEqual(["not_started", "blocked", "stalled", "live_idle", "graduated", "progressing"].map((signal) => needsFollowUp(signal as never)), [true, true, true, true, false, false]);
});

test("the nudge budget counts every send that may have gone out and holds the gap", () => {
  const policy = defaultOnboardingPolicy;
  assert.deepEqual(nudgeBudget(policy, 0, undefined, now), { remaining: 3, nextAllowedAt: null });
  assert.deepEqual(nudgeBudget(policy, 1, new Date(now - 2 * 86_400_000).toISOString(), now), { remaining: 2, nextAllowedAt: new Date(now + 86_400_000).toISOString() });
  assert.deepEqual(nudgeBudget(policy, 3, undefined, now).remaining, 0);
  assert.equal(windowEndOf(usage({ approvedAt: "-5d" }), policy, 0), now - 5 * 86_400_000 + 30 * 86_400_000);
});

test("the record reads sends, unknown sends and check-ins only from structured records", () => {
  const record = (tool: string, outcome: ToolRecord["outcome"], output: string, position: number, source: ToolRecord["source"] = "result"): ToolRecord => ({ tool, outcome, output, position, source });
  const history = {
    isComplete: true,
    records: [
      record("post_onboarding_report", "succeeded", '{"thread":{"ts":"1730000500.000100"}}', 2),
      record("draft_nudge", "succeeded", '{"nudge":{"number":1},"email":{"subject":"s","body":"b"}}', 4),
      record("send_nudge", "succeeded", '{"nudge":{"number":1},"sentAt":"2026-11-20T00:00:00.000Z"}', 6),
      record("draft_nudge", "succeeded", '{"nudge":{"number":2},"email":{"subject":"s2","body":"b2"}}', 8),
      record("send_nudge", "unknown", "", 10),
      // A note can never fake a thread or a draft.
      record("draft_nudge", "succeeded", '{"nudge":{"number":9},"email":{"subject":"x","body":"x"}}', 11, "note"),
      record("schedule_check_in", "succeeded", '{"nextCheckInAt":"2026-11-25T00:00:00.000Z"}', 12),
    ],
  };
  const result = onboardingRecordOf(history, { messages: [], queuedMessages: [] }, ["[check-in 2026-11-22T00:00:00.000Z] usage", "[slack 1.2] <@U>: send"]);
  assert.equal(result.thread?.ts, "1730000500.000100");
  assert.equal(result.latestNudge?.number, 2);
  assert.equal(result.nudgesSent, 2);
  assert.equal(result.lastNudgeSentAt, "2026-11-20T00:00:00.000Z");
  assert.equal(result.nextCheckInAt, "2026-11-25T00:00:00.000Z");
  assert.deepEqual([...result.deliveredCheckIns], ["2026-11-22T00:00:00.000Z"]);
  assert.equal(nudgeSendOf(history, 1), "sent");
  assert.equal(nudgeSendOf(history, 2), "unknown");
  assert.equal(nudgeSendOf({ isComplete: false, records: [] }, 3), "unknown");
});

test("counts the server cannot give are null: shown as n/a and never a signal by themselves", async () => {
  const { metricsLines } = await import("../src/onboarding-agent.ts");
  const nulls = { agentMessages7d: null, agentMessages28d: null, previewStarts7d: null, previewFailures7d: null, triggerEvents7d: null };
  const live = usage({ approvedAt: "-20d", lastActivityAt: "-1d", milestones: { firstSignInAt: "-19d", firstLiveAt: "-10d" }, counts: nulls });
  const assessment = assessUsage(live);
  assert.equal(assessment.signal, "graduated");
  const lines = metricsLines(live, assessment).join("\n");
  assert.match(lines, /\*Studio messages:\* n\/a \(7d\)/);
  assert.match(lines, /\*Previews:\* n\/a started, n\/a failed/);
  assert.doesNotMatch(lines, /Trigger events/);
  assert.equal(assessUsage(usage({ approvedAt: "-6d", lastActivityAt: "-1h", milestones: { firstSignInAt: "-5d" }, counts: nulls })).signal, "progressing");
});
