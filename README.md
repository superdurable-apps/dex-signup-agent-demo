# Dex signup review agent

A demo of the [Dex](https://docs.superdurable.io) agent framework on a real business process. When
someone signs up for Dex Cloud, an agent researches the applicant, recommends approving or rejecting
the account, and drafts a personal welcome email. It posts all of that to Slack, where admins
decide, ask for changes to the email, and finally say `send`.

## What it does

- **Research.** For each new pending sign-up, the agent looks the applicant up on GitHub and on their profile
  page or website.
- **Review post.** It posts a recommendation with its reasons and a first welcome email draft in the Slack review
  channel.
- **Review in the thread.** Listed reviewers reply in the post's thread:
  - `approve`: approves the account in Dex Cloud.
  - `reject <reason>`: rejects it.
  - `hold`: waits.
  - Anything else is feedback on the email, and the agent posts a new draft.
  - `send`, once the account is approved: emails the latest draft to the applicant from the team's Gmail.
- **Outcomes in the thread.** The approve, reject and send tools post their outcome in the thread themselves.
  The app caps each Slack post and email at one send, so a crash or a lost answer never posts or sends twice.
- **End.** The agent posts a summary and ends the review.

The application is the agent's **prompt and tools**. One Dex agent session per sign-up runs the process from the
first lookup to the sent email. Dex keeps it durable, so a crash, a deploy or a week of waiting for an admin loses
nothing and repeats nothing.

## Architecture

```text
 Dex Cloud ──list-pending──▶ SignupWatcher ──starts──▶ agent session  signup-review-<accountId>   (the only Flow)
 (ReviewAPI)                  (every 30 s)              │  github_user, fetch_profile
     ▲                                                  │  post_review ─────────────────────────────▶ Slack thread
     │  approve_account / reject_account                │                                                │ a reviewer replies
     └──────────────────────────────────────────────────┤  waits (a durable Dex wait, no process)        ▼
                                                        │  "[slack <ts>] <@U>: approve" ◀── threadReplyCreated Trigger
                                                        │                send_user_message RPC     (Socket Mode, or the
                                                        │                                           platform's relay)
                                                        │  post_draft, reply_in_thread ─────────────▶ Slack thread
                       Gmail ◀── send_welcome_email ────┘  finish_review
```

- **The agent session** ([`src/signup-agent.ts`](src/signup-agent.ts)): the prompt and nine tools. The tools that
  post to Slack or send email are Dex connector tools of the Slack and Gmail connectors, each call in its own
  Step.
- **SignupWatcher** ([`src/signup-watcher.ts`](src/signup-watcher.ts)): a loop that starts one session per pending
  sign-up, with the sign-up data as the first message. It does nothing else.
- **The reply Trigger** ([`src/review-replies.ts`](src/review-replies.ts)): the Slack connector's
  `threadReplyCreated` Trigger. It keeps each reviewer reply in a durable inbox, finds the review by its thread, and
  sends the reply to that session as `[slack <ts>] <@user>: <text>`.
- **Two harnesses**: pi ([`src/pi-agent.ts`](src/pi-agent.ts), Dex's built-in agent loop with pi-ai) and the Claude
  Agent SDK ([`src/claude-agent.ts`](src/claude-agent.ts)). Both use the same prompt, tools and Trigger.

A review goes like this:

1. The watcher starts the session.
2. The agent researches the applicant, calls post_review and waits. Waiting holds no process.
3. A reviewer's reply wakes the session for one turn. The agent approves, rejects, posts a new draft or sends the
   email.
4. After the send or the rejection, finish_review ends the session: it closes as `COMPLETED` once that turn ends.
   This needs server-zd 0.1.34 or newer and every Worker on `@superdurable/dex-agents` 0.6.14
   ([docs/design.md](docs/design.md#ending-a-review)).

Locally, one process runs the Dex Worker, the watcher and the Trigger. On Superverse, `node . worker` runs the Worker
and the watcher, and `node . web` runs the Trigger.

[docs/design.md](docs/design.md) covers the tools, the send and resend policy, de-duplication, reply routing and the
limits.

## Run it locally

You need Node.js 22.19 or later (24 recommended, see `.nvmrc`) and
[`dexcli`](https://docs.superdurable.io/components/run-locally) 1.43 or later.

1. **Set up a Slack app** at <https://api.slack.com/apps>:
   - **OAuth & Permissions**: bot scopes `chat:write` and `channels:history` (`groups:history` for a private
     review channel).
   - **Socket Mode**: on, with an app-level token with the `connections:write` scope (`xapp-…`).
   - **Event Subscriptions**: on, with the bot events `message.channels` and `message.groups`. Slack delivers them
     over Socket Mode, so the app needs no public URL.
   - Invite the app to the review channel (`/invite @<app>`).

   Use one Slack app per running copy of the demo. Slack delivers each event to only one of an app's Socket Mode
   connections, so two copies that share an app token each miss some of the replies.
2. **Start Dex** in its own terminal: `dexcli dev`. Dex Web opens on the address it prints.
3. **Create two connections** in Dex Web under **Connectors** > **+ Add**, with exactly these names:

   | Name | Connector | Authorization |
   | --- | --- | --- |
   | `review-slack` | Slack | OAuth of your Slack app. Then enter the `xapp-…` token in the connection's `app_token` field (under **Optional settings**). With the token, the reply Trigger uses Socket Mode. |
   | `welcome-gmail` | Gmail | Google OAuth of the mailbox that sends the emails (`gmail.send`, `gmail.readonly`, `userinfo.email`). Create a Web application OAuth client in Google Cloud, enable the Gmail API, and add the redirect URI Dex Web shows. |

   Dex Web saves them to `~/.dex/connectors/connections.json`; copy its path from **Local store**.
4. **Configure:** `cp .env.example .env`, then fill in:
   - `REVIEW_API_URL` and `REVIEW_API_KEY` (Dex Cloud staging), and `SIGNUP_REVIEW_SINCE`.
   - `SLACK_REVIEW_CHANNEL` and `SIGNUP_REVIEWER_SLACK_IDS`.
   - `DEX_CONNECTOR_CONFIG_FILE`: the path of `connections.json`.
   - The harness and model:
     - `SIGNUP_AGENT_HARNESS=pi` (the default) with `ANTHROPIC_API_KEY` for the default model
       `anthropic/claude-opus-5-5`.
     - Or, for an OpenAI- or Anthropic-compatible endpoint: `SIGNUP_AGENT_PI_BASE_URL`, `SIGNUP_AGENT_PI_MODEL`
       (the endpoint's model ID), `SIGNUP_AGENT_PI_API_KEY` and, for the Messages API,
       `SIGNUP_AGENT_PI_PROTOCOL=anthropic`.
     - Or `SIGNUP_AGENT_HARNESS=claude-agent-sdk`, after signing in to Claude Code once (`claude`, then `/login`),
       or with `SIGNUP_AGENT_CLAUDE_AUTH=api-key` and `ANTHROPIC_API_KEY`.

   An inconsistent model setting stops the start with a message that says what to set. All variables and the model
   rules are in [docs/reference.md](docs/reference.md#configuration).
5. **Start the agent:** `npm ci && npm start`. It reviews every pending sign-up registered since
   `SIGNUP_REVIEW_SINCE`. Stop it with Ctrl+C at any time. The sessions wait in Dex, and replies already delivered
   to the inbox continue after a restart. If a reply's delivery failed while the agent was down, the admin must
   post it again.
6. **Review in Slack:** reply in the review's thread with `approve`, `reject <reason>`, or what to change in the
   email; after the approval, `send`. Watch each session `signup-review-<accountId>` in Dex Web.

Each harness has its own session Flow type. Finish the open reviews before you switch harnesses, or switch back to
let them finish.

**Tests:** `npm test` runs the scenario suite of both harnesses, and `make test-app` runs the app tests. Neither
calls a real model, Slack, Gmail or GitHub. See [docs/reference.md](docs/reference.md#tests).

## Run it on Superverse

See [docs/superverse.md](docs/superverse.md). It covers connecting the repository, the connections and the reply
Trigger binding in Setup, the secrets and model settings, the build and processes, Preview and Live, regenerating
the session graph, and the hosted constraints.

## License

MIT, see [LICENSE](LICENSE).
