// The key usage metrics of an account, computed in code, never by the model: how far it got, how long since it last
// did anything, and whether it stalled and how. The agent writes the insights and the follow-up from them.

import type { AccountUsage, Milestones } from "./usage-api.ts";

/** The steps of getting started, in order; an account's stage is the last one it reached. */
export const onboardingStages = ["approved", "signed_in", "project", "building", "preview", "live"] as const;
export type OnboardingStage = (typeof onboardingStages)[number];

/** Which milestone reaches which stage; a later milestone reaches its stage even when an earlier one is missing. */
const stageMilestones: readonly [OnboardingStage, (keyof Milestones)[]][] = [
  ["signed_in", ["firstSignInAt"]],
  ["project", ["teamSpaceCreatedAt", "projectCreatedAt"]],
  ["building", ["firstAgentMessageAt", "repositoryLinkedAt", "firstConnectionAt"]],
  ["preview", ["firstPreviewReadyAt"]],
  ["live", ["firstLiveAt"]],
];

/**
 * What the usage says, as the agent's check-in gets it:
 * - `not_started`: never signed in, `notStartedAfterDays` or more after the approval.
 * - `blocked`: Preview starts keep failing (2 or more in 7 days) and no Preview was ever ready.
 * - `stalled`: not live, and no activity for `stalledAfterDays` or more.
 * - `live_idle`: live, but neither activity nor Trigger events in 7 days.
 * - `graduated`: live for `graduatedAfterDays` or more, with activity or Trigger events in the last 7 days.
 * - `progressing`: anything else.
 */
export type UsageSignal = "not_started" | "blocked" | "stalled" | "live_idle" | "graduated" | "progressing";

export interface StallRules {
  readonly notStartedAfterDays: number;
  readonly stalledAfterDays: number;
  readonly graduatedAfterDays: number;
}
export const defaultStallRules: StallRules = { notStartedAfterDays: 2, stalledAfterDays: 4, graduatedAfterDays: 7 };

export interface UsageAssessment {
  readonly stage: OnboardingStage;
  /** 1 to 6: the stage's place in {@link onboardingStages}. */
  readonly stageNumber: number;
  readonly signal: UsageSignal;
  /** Whole days since the approval, the last activity and the stage's milestone; null when unknown. */
  readonly daysSinceApproval: number | null;
  readonly daysSinceActivity: number | null;
  readonly daysInStage: number | null;
  /** The next step to reach, or null once live. */
  readonly nextStage: OnboardingStage | null;
  /** The latest failed Preview start's message, when the latest start failed. */
  readonly lastPreviewFailure: string | null;
}

const dayMs = 24 * 60 * 60 * 1_000;
const daysBetween = (from: string | null, to: number): number | null => {
  const at = from === null ? Number.NaN : Date.parse(from);
  return Number.isNaN(at) ? null : Math.max(0, Math.floor((to - at) / dayMs));
};

export function assessUsage(usage: AccountUsage, rules: StallRules = defaultStallRules): UsageAssessment {
  const now = Date.parse(usage.asOf);
  let stage: OnboardingStage = "approved";
  let stageAt: string | null = usage.approvedAt;
  for (const [candidate, keys] of stageMilestones) {
    const reached = keys.map((key) => usage.milestones[key]).filter((value): value is string => value !== null).sort();
    if (reached.length > 0) {
      stage = candidate;
      stageAt = reached[0]!;
    }
  }
  const stageNumber = onboardingStages.indexOf(stage) + 1;
  const daysSinceApproval = daysBetween(usage.approvedAt, now);
  const daysSinceActivity = daysBetween(usage.lastActivityAt, now);
  const recentlyActive = (daysSinceActivity ?? Number.POSITIVE_INFINITY) < 7 || usage.counts.triggerEvents7d > 0;
  const failure = usage.projects.find((project) => project.lastPreviewFailure !== null)?.lastPreviewFailure ?? null;
  let signal: UsageSignal = "progressing";
  if (stage === "live") {
    const daysLive = daysBetween(usage.milestones.firstLiveAt, now) ?? 0;
    signal = !recentlyActive ? "live_idle" : daysLive >= rules.graduatedAfterDays ? "graduated" : "progressing";
  } else if (usage.milestones.firstSignInAt === null && (daysSinceApproval ?? 0) >= rules.notStartedAfterDays) {
    signal = "not_started";
  } else if (usage.milestones.firstPreviewReadyAt === null && usage.counts.previewFailures7d >= 2) {
    signal = "blocked";
  } else if (usage.milestones.firstSignInAt !== null && (daysSinceActivity ?? Number.POSITIVE_INFINITY) >= rules.stalledAfterDays) {
    signal = "stalled";
  }
  return {
    stage,
    stageNumber,
    signal,
    daysSinceApproval,
    daysSinceActivity,
    daysInStage: daysBetween(stageAt, now),
    nextStage: onboardingStages[stageNumber] ?? null,
    lastPreviewFailure: failure === null ? null : failure.message,
  };
}

/** The signals that call for a follow-up to the customer. */
export const needsFollowUp = (signal: UsageSignal): boolean => signal === "not_started" || signal === "blocked" || signal === "stalled" || signal === "live_idle";

/** The usage and its assessment as the agent reads them: data, not instructions. */
export function usageFacts(usage: AccountUsage, assessment: UsageAssessment): Record<string, unknown> {
  return {
    assessment,
    lastSignInAt: usage.lastSignInAt,
    lastActivityAt: usage.lastActivityAt,
    activeDays7d: usage.activeDays7d,
    activeDays28d: usage.activeDays28d,
    milestones: usage.milestones,
    counts: usage.counts,
    connectors: usage.connectors,
    projects: usage.projects,
    asOf: usage.asOf,
  };
}
