# Application Instructions

Dex `node-ts-v1`, TypeScript on Node.js 22: a pi agent on Dex Agents that reviews Dex Cloud sign-ups with the
admins in Slack. One agent session per sign-up (`signup-review-<accountId>`) is the only Flow. A watcher in the
Worker process starts each pending sign-up's session; the Slack connector's `threadReplyCreated` Trigger, in the
web process, delivers each reviewer reply to its session through the session Flow's `send_user_message` RPC. The
README gives the overview and the local run; `docs/design.md` describes the design, the send policy and the limits,
`docs/dex-cloud.md` the hosted setup, and `docs/reference.md` the configuration, commands, files and tests.

## Files to change for a feature

1. `src/signup-agent.ts`: the agent's prompt and tools. The tools that post or send are Dex connector tools; each
   reads the review's thread and latest draft from the session's own tool records (`src/review-state.ts`).
2. `src/review-replies.ts`: the reply Trigger's target and runner, and the index from a thread to its session.
3. `src/signup-watcher.ts`: starts each pending sign-up's session; nothing else.
4. `src/application.ts`: `loadApplication`, the configuration (`src/config.ts`) and each mode's background work.
5. `apptest/signup-review.test.ts`: the app tests; `test/` holds the scenario suite of the pi and Claude Agent SDK
   harnesses.
6. `eino/`: the Eino harness, a Go module that ports `src/` file by file (`eino/internal/signup`), with its own
   scenario suite and crash tests (`eino/scenarios`). A change to the prompt, a tool, the reply routing or the
   watcher changes both: copy `signupAgentPrompt` into `eino/internal/signup/prompt.txt` (a test fails while they
   differ), and port the change to the Go file of the same name. The connector tools of `createSignupConnectorTools`
   (their checks, texts, schemas and descriptions) are in `connector_tools.go`, and `connector_binding.go` runs them
   as connector operations; no test compares them with the TypeScript tools.

The session Flow's graph is committed, not rendered: dex-app.yaml declares
`flow-definitions/signup-review-pi-agent.json`. When the Flow type or permission changes, or after a
`@superdurable/dex-agents` update, or a change to the Slack reply binding or the Slack connector version, write
both harnesses' graphs again with `make session-graphs`; it passes
`--trigger-bindings scripts/session-trigger-bindings.json`, so each graph declares the `signup-review-reply` binding
in `v2.connectorTriggerBindings`, which Dex Cloud Setup requires. `make check-fdg-v2` fails while a graph differs
from the generator's output or its binding differs from `dex-app.yaml`. Never edit a graph by hand, and never rename
the Flow type of a session with open reviews.

The base files `src/main.ts`, `src/dex.ts`, `src/runtime.ts`, `src/http.ts` and `apptest/apptest.ts` follow the
platform contract: `npm run build` writes `dist/`, `node . web` serves `/api/health` and the relayed Slack events on
`PORT`, `node . worker` runs the Worker and the watcher with readiness on `/readyz`, both read
`/tmp/superverse-app/runtime.yaml`, write only under `/tmp` and stop on SIGTERM, the Worker after draining its tasks.
`node .` without a mode is the local process, which runs both roles. Keep them.

## Configuration

- Hosted, the application environment comes from the Project configuration (`dex-app.yaml`
  `application.environment`): `REVIEW_API_URL`, the secret `REVIEW_API_KEY`, `SIGNUP_REVIEW_SINCE`, the model, and
  the optional ones. The model is the secret `ANTHROPIC_API_KEY` for an `anthropic/…` `SIGNUP_AGENT_PI_MODEL` (the
  default), or `SIGNUP_AGENT_PI_BASE_URL` with the endpoint's model ID in `SIGNUP_AGENT_PI_MODEL` and the secret
  `SIGNUP_AGENT_PI_API_KEY`, with the optional `SIGNUP_AGENT_PI_PROTOCOL` `openai` (default) or `anthropic` (the
  older `openai-compatible/<model ID>` still works with `openai`); `src/config.ts` (`piModelSelection`)
  holds the rules and refuses inconsistent combinations at start. It is applied to a copy of the process environment, never to
  `process.env`. The review channel and the reviewers come only from the `signup-review-reply` binding's
  configuration, which the user fills in Studio Setup.
- Locally, `.env` sets everything, including `SLACK_REVIEW_CHANNEL` and `SIGNUP_REVIEWER_SLACK_IDS`.
- Only the pi harness runs hosted; the Claude Agent SDK and Eino harnesses run only locally. The Eino process reads
  the same `.env`, with its model in `SIGNUP_AGENT_EINO_*`.
- The connector entries of `dex-app.yaml` name npm packages with the exact versions `package.json` pins (the
  platform's PGTSCONN shape); `make check-fdg-v2` fails when they differ.
- `dex-app.yaml` is strict YAML: no anchors, aliases, tags or repeated keys, and quote every value YAML 1.1 and 1.2
  read differently (a version such as `"0.7.0"`, `yes`/`no`/`on`/`off`, a leading zero, a date, a signed or
  floating-point number). `make check-fdg-v2` reads it through `tools/appmanifest`, the platform's own decoder (Go);
  never edit `tools/appmanifest`.

## Rules

- Keep the agent a harness configuration: prompt, tools and their effects. Add a Flow only for work outside a
  session.
- `package-lock.json` pins every dependency: change dependencies with `npm install --save-exact`, commit the
  lockfile, never commit `node_modules` or `dist`. No package script runs in the build. Keep one `@superdurable/dex` and
  one `@superdurable/connectors-sdk` for every package (`npm ls @superdurable/dex @superdurable/connectors-sdk`):
  `@superdurable/dex` is pinned to the version the dex-agents packages peer, and `@superdurable/connectors-sdk` to
  the version the connector packages depend on.
- finish_review ends a session with `StopType.GRACEFUL_COMPLETE`, so it closes `COMPLETED`: keep server-zd at 0.1.34
  or newer and every Worker of the session Flow type on `@superdurable/dex-agents` 0.6.14 or newer. Keep a stop of
  an idle session (a person in Dex Web) a cancel; see docs/design.md, Ending a review.
- Never put a key in source, Flow state, logs or responses.

## Tests

- `make test-app` runs `apptest/*.test.ts`, each test on its own `dex-server-zd dev`, with a scripted pi model, the
  connector mocks, a ReviewAPI stub, and reviewer replies as Slack events on a Socket Mode stand-in.
- `npm test` runs the scenario suite of the pi and Claude Agent SDK harnesses (`test/`); it needs `dexcli` and
  `dex-server-zd` on `PATH`.
- `make test-eino` runs the Eino harness's tests (`eino/`) with Go 1.26 or later and `DEXCLI` (or `dexcli` on `PATH`).
  Until Dex releases its Go modules `agents/go/core` and `agents/go/eino`, `eino/go.mod` replaces them with
  `.local/dex`, the Dex checkout that `EINO_DEX_CHECKOUT` names once (`make test-eino EINO_DEX_CHECKOUT=<path>`
  links it there). Never put a path of your machine in `eino/go.mod`; a test fails on one.
- Wait with `env.eventually` or `pollUntil`, never call a real model.

## Commands

- `make bootstrap` restores `node_modules`; `make check-static` checks the Flow graph, the types and the build.
- `make test-app RUN=<regexp>` runs app tests; `make test` runs the scenario suite.
- `make run-eino` runs the Eino harness locally; `make test-eino RUN=<regexp>` runs its tests. Both take
  `EINO_DEX_CHECKOUT=<path>` the first time.
