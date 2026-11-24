# Onboarding wizard

A second agent in this application. Once the signup review approves an account, the onboarding wizard takes over. It
tracks the account's usage on Dex Cloud, reports the key metrics and insights to the admins in one Slack thread per
account, and, when usage stalls, drafts a personal follow-up email that an admin approves in the thread. It runs on
the [Vercel AI SDK](https://ai-sdk.dev) through the Dex Vercel AI adapter (`@superdurable/dex-agents-vercel-ai`), so
each model call and each tool call is a Dex Step, and an onboarding that waits for days holds no process.

It is off unless `ONBOARDING_ENABLED=true`, so a deployment that has not configured it runs as before.

## Lifecycle

```text
 signup review ── approve_account ──starts──▶ onboarding-<accountId>     (Flow type OnboardingWizardVercelAiFlow)
                   (fixed ID, once)            │  get_usage
 OnboardingWatcher ─ backfill: accounts ─────▶ │  post_onboarding_report ───────────────▶ Slack, review channel (new thread)
   (every 60 s)      approved elsewhere        │  schedule_check_in(days)                      │ admins reply: send, skip,
                                               │  waits (a durable Dex wait, no process)       │ edits, check in <n> days, stop
                   ─ due check-in: ──────────▶ │  "[check-in <time>] usage + assessment"       ▼
                     fresh usage, budget       │  post_report_update, draft_nudge? ◀── reply Trigger (same binding)
                                               │  "[slack <ts>] <@U>: send" ─▶ send_nudge ──▶ Gmail (welcome-gmail)
                                               │  schedule_check_in … finish_onboarding
```

1. **Handoff.** `approve_account`, after the approval succeeds, starts `onboarding-<accountId>` with the sign-up data,
   the latest welcome email draft and the review thread. The ID is fixed and never reused, so a retried Step starts
   no second session. The start is best effort: a failure only logs, and the watcher starts the onboarding later
   without the review's findings. Only the TypeScript harnesses (pi, Claude Agent SDK) hand off. Accounts that the
   Eino harness approves, or that an admin approves in Dex Cloud admin, are started by the watcher's backfill, 10
   minutes after their approval.
2. **First report.** The agent reads the usage and posts a top-level message in the review channel, which opens the
   account's onboarding thread. Then it schedules its first check-in.
3. **Check-ins.** The agent picks its own cadence with `schedule_check_in`, within 1 to 14 days. The watcher keeps the
   appointment. When the time has come and the session is idle with nothing queued, the watcher sends
   `[check-in <time>] …` with fresh usage, its assessment, the nudge budget and the window. The request ID is the
   appointment's, so a repeat delivers it once. If a check-in turn ends without scheduling the next one, the watcher
   adds a check-in a day later, at most twice.
4. **Follow-ups.** For `not_started`, `blocked`, `stalled` or `live_idle`, the agent drafts a nudge in the thread. An
   admin replies `send`, `skip`, or an edit. Nothing reaches a customer without an admin's `send`.
5. **End.** On `graduated`, an inactive account, the end of the window (30 days after the approval), or an admin's
   `stop`, the agent posts a summary and calls `finish_onboarding`. The session completes when that turn ends.

The onboarding posts in the review channel through the same Slack app, so the one `signup-review-reply` binding and
its reviewers serve both agents. A reply goes to the running review that posted its thread, else to the running
onboarding that did.

Why the watcher and not the core's `durable_wait`: a session parked in a durable wait holds its turn open, so an
admin's `send` would queue until the timer fires. Here every turn ends, the session waits for any message, and the
next check-in is an appointment in the session's own records that the watcher reads.

## Metrics: code computes, the model explains

[`src/onboarding-metrics.ts`](../src/onboarding-metrics.ts) turns the usage into the assessment. The model is told
never to recompute or contradict it, and the report tools render the metrics block themselves.

| Field | Rule |
| --- | --- |
| Stage | The last of approved, signed_in, project (TeamSpace or project), building (Studio message, linked repository or connection), preview (a Preview ready), live (first Live) that the milestones reach |
| `not_started` | Never signed in, 2 or more days after the approval |
| `blocked` | 2 or more Preview failures in 7 days and no Preview ever ready |
| `stalled` | Signed in, not live, no activity for 4 or more days |
| `live_idle` | Live, but no activity and no Trigger events in 7 days |
| `graduated` | Live for 7 or more days, with activity or Trigger events in the last 7 days |
| `progressing` | Anything else |

The report shows the stage, the signal, the day of the onboarding, the days since the last activity, active days
(7 and 28), Studio messages, projects and Live projects, connections and their connectors, Preview starts and
failures, publishes, Trigger events, members, and the latest Preview failure.

## Tools

| Tool | Effect | What the tool enforces |
| --- | --- | --- |
| `get_usage` | read | The session's own account only |
| `schedule_check_in` | idempotent write, records only | 1 to 14 days; never past the window's end, so the last check-in can wrap up |
| `finish_onboarding` | idempotent write | Graceful completion, as `finish_review`; the agent's `stopWhen` ends the turn right after it, so no model call follows |
| `post_onboarding_report` | Slack channel post | Once; refuses after a post that may have gone out |
| `post_report_update` | Slack thread reply | Adds the current metrics |
| `draft_nudge` | Slack thread reply | Refuses with no budget left or an unchanged draft; numbers the drafts |
| `reply_in_thread` | Slack thread reply | Escapes Slack markup |
| `send_nudge` | Gmail send | Sends the latest draft to the account's email; refuses for an inactive account, a draft already sent or possibly sent, an exhausted budget (3 by default), or less than 3 days since the last nudge |

The connector tools run as in the review: one fenced connector write per tool Step, the connector's send cap, and
every send that did not definitely fail counts against the budget. As in the review, a thread or a draft counts only
from a structured tool record, never from a turn note.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `ONBOARDING_ENABLED` | `false` | `true` turns the wizard on |
| `ONBOARDING_SINCE` | required when on | Only accounts approved since this RFC 3339 time are onboarded |
| `ONBOARDING_USAGE_API_URL`, `ONBOARDING_USAGE_API_KEY` | `REVIEW_API_URL`, `REVIEW_API_KEY` | The usage operations ([docs/usage-api.md](usage-api.md)) |
| `ONBOARDING_AGENT_MODEL` | `claude-opus-5-5` | An Anthropic model, with `ANTHROPIC_API_KEY` |
| `ONBOARDING_AGENT_BASE_URL`, `ONBOARDING_AGENT_API_KEY` | none | An OpenAI-compatible endpoint instead; `ONBOARDING_AGENT_MODEL` is its model ID |
| `ONBOARDING_WATCH_INTERVAL_SECONDS` | `60` | Seconds between two watcher rounds |
| `ONBOARDING_MAX_NUDGES`, `ONBOARDING_MIN_NUDGE_GAP_DAYS`, `ONBOARDING_WINDOW_DAYS` | `3`, `3`, `30` | The follow-up limits |

## Running it locally

Dex Cloud does not serve the usage operations yet. Run the stand-in next to the agent:

```bash
npm run usage-standin            # prints ONBOARDING_USAGE_API_URL and ONBOARDING_USAGE_API_KEY for .env
```

It answers from `.local/usage-standin.json` and reads the file again on every request. An account it does not know
appears there at its first read, as just approved and unused. Edit its members to play a story: for a stalled
account, set `"milestones": {"firstSignInAt": "-5d"}` and `"lastActivityAt": "-5d"`. Times may be relative (`-3d`,
`-6h`, `now`). Check-ins are at least a day apart. To see the agent react sooner, ask in the thread, such as
`how is this account doing?`: the agent reads fresh usage and answers there.

## Limits

- The usage operations are a proposal (docs/usage-api.md). Until Dex Cloud serves them, a hosted Project with the
  wizard on needs a reachable stand-in, which only makes sense for a demo.
- The watcher reads every running onboarding's history each round, which suits hundreds of accounts. For many more,
  the check-in appointment would move to a Dex Timer of its own.
- The admins approve every nudge. The tools cap the nudges whatever the model asks, but whether a `send` came from an
  admin rests, as for the welcome email, on the reply Trigger's reviewer list.
- The Eino harness does not hand off; the watcher's backfill covers its approvals.
