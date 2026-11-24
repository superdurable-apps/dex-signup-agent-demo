// What the onboarding tools, the watcher and the reply Trigger share: the onboarding session's FlowID, and its Slack
// thread, nudge drafts, nudge sends and next check-in as the session's own tool records hold them. Like a review, an
// onboarding keeps no state of its own outside its session.

import type { AgentSnapshot } from "@superdurable/dex-agents";

import { ReviewSessions, toolRecordsOf, type NoteToolKinds, type ReviewSessionFlow, type ReviewThread, type ToolHistory, type ToolRecord } from "./review-state.ts";

export const onboardingSessionId = (accountId: string): string => `onboarding-${accountId}`;
export const onboardingAccountIdOf = (sessionId: string): string => sessionId.replace(/^onboarding-/, "");

/** A turn note may add evidence of a nudge send and a possible report post; no decision tools. */
export const onboardingNoteToolKinds: NoteToolKinds = { sendTool: "send_nudge", decisionTools: [], postOnceTool: "post_onboarding_report" };

/** A follow-up email draft posted in the onboarding thread; the latest one is what send_nudge sends. */
export interface NudgeDraft {
  readonly number: number;
  readonly subject: string;
  readonly body: string;
}

/** What the session's own tool records say about its onboarding. */
export interface OnboardingRecord {
  /** The thread of the succeeded post_onboarding_report, or `undefined` before it. */
  readonly thread: ReviewThread | undefined;
  /** Whether a post_onboarding_report may have posted: one did not definitely fail, or the history read is incomplete. */
  readonly isReportPosted: boolean;
  readonly latestNudge: NudgeDraft | undefined;
  /** Nudge sends that succeeded or may have: each counts against the nudge budget. */
  readonly nudgesSent: number;
  /** When the latest nudge that surely went out was sent. */
  readonly lastNudgeSentAt: string | undefined;
  /** The latest succeeded schedule_check_in's time, or `undefined` before one. */
  readonly nextCheckInAt: string | undefined;
  /** The check-in times whose message the session already holds or has queued. */
  readonly deliveredCheckIns: ReadonlySet<string>;
  readonly history: ToolHistory;
}

const objectOf = (text: string): Record<string, unknown> | undefined => {
  try {
    const value: unknown = JSON.parse(text);
    return typeof value === "object" && value !== null && !Array.isArray(value) ? (value as Record<string, unknown>) : undefined;
  } catch {
    return undefined;
  }
};
const threadOf = (output: string): ReviewThread | undefined => {
  const thread = objectOf(output)?.thread as { ts?: unknown } | undefined;
  return typeof thread?.ts === "string" && /^\d+\.\d+$/.test(thread.ts) ? { ts: thread.ts } : undefined;
};
const nudgeOf = (output: string): NudgeDraft | undefined => {
  const value = objectOf(output);
  const nudge = value?.nudge as { number?: unknown } | undefined;
  const email = value?.email as { subject?: unknown; body?: unknown } | undefined;
  return typeof nudge?.number === "number" && typeof email?.subject === "string" && typeof email.body === "string"
    ? { number: nudge.number, subject: email.subject, body: email.body }
    : undefined;
};
const structured = (record: ToolRecord) => record.source !== "note" && record.outcome === "succeeded";

/** The check-in message the watcher sends: `[check-in <RFC 3339 time>] ...`. */
export const checkInPrefix = (at: string): string => `[check-in ${at}]`;
const checkInOf = (text: string | null | undefined): string | undefined => /^\[check-in (\d{4}-\d{2}-\d{2}T[^\]]+)\]/.exec(text ?? "")?.[1];

/**
 * The onboarding as the session's tool records hold it. As for a review, the thread and drafts count only from a
 * structured record, never from a turn note, and every send that did not definitely fail counts against the budget.
 */
export function onboardingRecordOf(history: ToolHistory, snapshot: Pick<AgentSnapshot, "messages" | "queuedMessages">, userTexts: readonly string[]): OnboardingRecord {
  const { records } = history;
  const reports = records.filter((record) => record.tool === "post_onboarding_report");
  const thread = reports.find((record) => structured(record) && threadOf(record.output) !== undefined);
  const nudges = records.filter((record) => record.tool === "draft_nudge" && structured(record)).map((record) => nudgeOf(record.output)).filter((draft) => draft !== undefined);
  const sends = records.filter((record) => record.tool === "send_nudge" && record.outcome !== "failed");
  const sentAt = sends
    .filter(structured)
    .map((record) => objectOf(record.output)?.sentAt)
    .filter((value): value is string => typeof value === "string")
    .at(-1);
  const schedule = records.filter((record) => record.tool === "schedule_check_in" && structured(record)).at(-1);
  const nextCheckInAt = schedule === undefined ? undefined : objectOf(schedule.output)?.nextCheckInAt;
  const delivered = new Set<string>();
  for (const text of [...userTexts, ...(snapshot.queuedMessages ?? []).map((queued) => queued.message.text)]) {
    const at = checkInOf(text);
    if (at !== undefined) {
      delivered.add(at);
    }
  }
  return {
    thread: thread === undefined ? undefined : threadOf(thread.output),
    isReportPosted: reports.some((record) => record.outcome !== "failed") || (!history.isComplete && reports.length === 0),
    latestNudge: nudges.at(-1),
    nudgesSent: sends.length,
    lastNudgeSentAt: sentAt,
    nextCheckInAt: typeof nextCheckInAt === "string" ? nextCheckInAt : undefined,
    deliveredCheckIns: delivered,
    history,
  };
}

/**
 * Whether nudge `number` was sent, or may have been, after the call that posted it: as for a welcome email draft,
 * `sent`, `unknown` (an unknown outcome, or an incomplete history without the draft's post), or `undefined`.
 */
export function nudgeSendOf(history: ToolHistory, number: number): "sent" | "unknown" | undefined {
  const { records, isComplete } = history;
  const posted = records.filter((record) => record.tool === "draft_nudge" && structured(record) && record.output.includes(`"nudge":{"number":${number}}`));
  if (posted.length === 0 && !isComplete) {
    return "unknown";
  }
  const since = posted.at(-1)?.position ?? Number.NEGATIVE_INFINITY;
  const sends = records.filter((record) => record.tool === "send_nudge" && record.outcome !== "failed" && record.position > since);
  return sends.some((record) => record.outcome === "succeeded") ? "sent" : sends.length > 0 ? "unknown" : undefined;
}

/** The onboarding sessions: their records, their thread index for the reply Trigger, and ending them. */
export class OnboardingSessions extends ReviewSessions {
  public constructor() {
    super({ sessionIdOf: onboardingSessionId, accountOf: onboardingAccountIdOf, noteKinds: onboardingNoteToolKinds, noun: "onboarding" });
  }

  /** The onboarding's record, from one read of the session's history. */
  public async onboarding(accountId: string): Promise<OnboardingRecord & { snapshot: AgentSnapshot }> {
    const { snapshot, messages, isComplete } = await this.history(accountId);
    const history = { records: toolRecordsOf(messages, snapshot.toolCalls, onboardingNoteToolKinds), isComplete };
    const userTexts = messages.filter((message) => message.role === "user" && message.origin === undefined).map((message) => message.text ?? "");
    return { ...onboardingRecordOf(history, snapshot, userTexts), snapshot };
  }

  protected override async threadOfSession(accountId: string): Promise<ReviewThread | undefined> {
    return (await this.onboarding(accountId)).thread;
  }
}

export type OnboardingSessionFlow = ReviewSessionFlow;
