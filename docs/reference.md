# Reference

Configuration, commands, files and tests. For the overview, see the [README](../README.md).

## Configuration

Locally, `npm start` and `make run-eino` load `.env` (copy [`.env.example`](../.env.example)); a variable already set in
the shell wins. Hosted, the application environment
comes from the Project configuration; see [dex-cloud.md](dex-cloud.md#setup). [`src/config.ts`](../src/config.ts)
reads and checks it at start, and a bad value stops the start with a message that says what to set.

| Variable | Meaning |
| --- | --- |
| `DEX_FLOW_SERVICE_ADDRESS` | Dex server; `127.0.0.1:8801` (`dexcli dev`) by default |
| `DEX_TLS` | `true` or `1` for a TLS connection to a remote Dex server |
| `DEX_API_KEY`, `DEX_TOKEN_URL`, `DEX_SERVER_ID` | A remote Dex server: the API key exchanged for tokens. With `DEX_API_KEY`, the other two are required |
| `DEX_TOKEN_CA_FILE` | Optional: an extra CA the token exchange trusts (set by the platform where needed) |
| `DEX_BLOB_CACHE_DIR` | The blob cache directory: `.dex-blob-cache` locally by default; hosted, each process uses `<dir>/<mode>` |
| `REVIEW_API_URL`, `REVIEW_API_KEY` | Required. The Dex Cloud account review API (staging) and its key `srk_<id>_<secret>` |
| `SIGNUP_REVIEW_SINCE` | Required. RFC 3339 time, such as `2026-11-13T00:00:00Z`: only sign-ups registered since then are reviewed |
| `SLACK_REVIEW_CHANNEL` | Local only. Channel ID of the review posts and of the replies the Trigger takes |
| `SIGNUP_REVIEWER_SLACK_IDS` | Local only. Comma-separated Slack member IDs whose replies count |
| `DEX_CONNECTOR_CONFIG_FILE` | Local only. The connector configuration (`connections.json`) with `review-slack` and `welcome-gmail` |
| `SIGNUP_AGENT_HARNESS` | `pi` (default) or `claude-agent-sdk` for `npm start`. Hosted, only `pi`. The Eino process takes it unset or `eino`; `make run-eino` sets `eino` |
| `SIGNUP_AGENT_PI_MODEL` | pi: the model; see [pi model selection](#pi-model-selection). Default `anthropic/claude-opus-5-5` |
| `ANTHROPIC_API_KEY` | pi: required for an `anthropic/…` catalog model. Claude Agent SDK: used with `SIGNUP_AGENT_CLAUDE_AUTH=api-key` |
| `SIGNUP_AGENT_PI_BASE_URL`, `SIGNUP_AGENT_PI_API_KEY` | pi with a custom endpoint: the base URL selects it, and its key is then required |
| `SIGNUP_AGENT_PI_PROTOCOL` | Optional, only with `SIGNUP_AGENT_PI_BASE_URL`: `openai` (default, Chat Completions) or `anthropic` (Messages) |
| `SIGNUP_AGENT_CLAUDE_MODEL` | Claude Agent SDK: the model, `claude-opus-5-5` by default |
| `SIGNUP_AGENT_CLAUDE_AUTH` | Claude Agent SDK: `login` (default, your Claude Code login) or `api-key` |
| `SIGNUP_AGENT_CLAUDE_CLI` | Claude Agent SDK: optional path of a Claude Code CLI to run instead of the bundled one |
| `SIGNUP_AGENT_EINO_BASE_URL`, `SIGNUP_AGENT_EINO_MODEL`, `SIGNUP_AGENT_EINO_API_KEY` | Eino: the endpoint's base URL, model ID and key, all required; see [Eino model selection](#eino-model-selection) |
| `SIGNUP_AGENT_EINO_PROTOCOL` | Eino: `openai` (default, Chat Completions) or `anthropic` (Messages) |
| `GITHUB_TOKEN` | Optional. Raises the GitHub API rate limit for `github_user` |
| `SIGNUP_WATCH_INTERVAL_SECONDS` | Seconds between reads of the pending sign-ups (30) |
| `SIGNUP_WORKER_SLOTS` | Local only. The Worker's execution slots (64). A running turn or tool call takes one; a review that waits for a reply takes none. Hosted, the slots come from `application.workerSlots` in `dex-app.yaml` |
| `PORT` | The web process's port (8080 by default; the platform sets it) |
| `DEX_PROJECT_CONFIG_KEY` | Set by the platform. Its presence makes the process run as hosted |

### pi model selection

`piModelSelection` in [`src/config.ts`](../src/config.ts) holds the rules. Any other combination stops the start with
a message that says what to set.

- **No `SIGNUP_AGENT_PI_BASE_URL`:** `SIGNUP_AGENT_PI_MODEL` is `<provider>/<model ID>` of pi-ai's catalog, such as
  `anthropic/claude-opus-5-5` (the default).
  - An `anthropic/…` model requires `ANTHROPIC_API_KEY`. Locally, another catalog provider's key is read from its
    own variable, such as `OPENAI_API_KEY`; `dex-app.yaml` declares no such variable, so hosted the model is
    Anthropic's or a custom endpoint.
  - A value without a catalog provider (such as `openai` or `qwen3.8-flash-next`), an `openai-compatible/…` value,
    and `SIGNUP_AGENT_PI_PROTOCOL` are refused with a pointer to `SIGNUP_AGENT_PI_BASE_URL`.
- **`SIGNUP_AGENT_PI_BASE_URL` set:** `SIGNUP_AGENT_PI_MODEL` and `SIGNUP_AGENT_PI_API_KEY` are required, and
  `SIGNUP_AGENT_PI_PROTOCOL` is `openai` unless set to `anthropic`. The base URL is an http(s) URL with a host and
  no query or fragment.
  - `openai`: the base URL is the part before `/chat/completions` (such as `https://llm.example.com/v1`). The model
    is the endpoint's model ID as it is, slashes included, such as `qwen3.8-flash-next` or `meta-llama/Llama-3`. A
    value that starts with a pi-ai catalog provider, such as `anthropic/…` or `openai/…`, is refused as ambiguous:
    write it with the prefix `openai-compatible/`, as in `openai-compatible/openai/gpt-oss-120b`. The older form
    `openai-compatible/<model ID>` keeps working for any ID. Thinking content streamed as `reasoning_content` is
    not sent back in later requests.
  - `anthropic`: the base URL is the part before `/v1/messages` (such as `https://llm.example.com/anthropic`). The
    model is the endpoint's model ID as it is, whatever it starts with; the `openai-compatible/` prefix is refused.
    The key is sent as `x-api-key`; `ANTHROPIC_API_KEY` is never used.
  - `ANTHROPIC_API_KEY` is not needed with either protocol.

### Eino model selection

`einoModelSelection` in [`eino/internal/signup/config.go`](../eino/internal/signup/config.go) holds the rules. They are
the pi rules for a custom endpoint, since Eino has no model catalog; any other combination stops the start with a
message that says what to set.

- `SIGNUP_AGENT_EINO_BASE_URL`, `SIGNUP_AGENT_EINO_MODEL` and `SIGNUP_AGENT_EINO_API_KEY` are required.
  `SIGNUP_AGENT_EINO_PROTOCOL` is `openai` unless set to `anthropic`. The base URL is an http(s) URL with a host and no
  query or fragment.
  - `openai` (eino-ext `components/model/openai`): the base URL is the part before `/chat/completions` (such as
    `https://llm.example.com/v1`), and the key goes as a Bearer token.
  - `anthropic` (eino-ext `components/model/claude`): the base URL is the part before `/v1/messages` (such as
    `https://llm.example.com/anthropic`), and the key goes as `x-api-key`; no `Authorization` header is sent, so an
    `ANTHROPIC_AUTH_TOKEN` of the shell never reaches the endpoint. The Anthropic Go SDK under eino-ext retries a failed
    request twice inside the model call, and eino-ext offers no option to turn that off.
- The model is the endpoint's model ID as it is, slashes included; the pi prefix `openai-compatible/` is refused.
- Both protocols ask for at most 16,384 output tokens.

## Commands

| Command | What it does |
| --- | --- |
| `npm ci` | Installs the pinned dependencies |
| `npm start` | Builds and runs the local process (Worker, watcher and reply Trigger) with `.env` |
| `npm run build` | Compiles `src/` to `dist/` |
| `npm run typecheck` | Type-checks sources and tests |
| `npm test` / `make test` | The scenario suite of both harnesses (`test/`) |
| `make bootstrap` | Downloads the Go module of `tools/appmanifest`, then restores `node_modules` from `package-lock.json` (`npm ci --ignore-scripts`), only when the lockfile changed |
| `make check-fdg-v2` | Reads `dex-app.yaml` through `tools/appmanifest`, then checks the committed session graph and the connector entries (see [dex-cloud.md](dex-cloud.md#session-graphs)) |
| `make check-static` | `check-fdg-v2`, then the type check and the build |
| `make test-app [RUN=<regexp>]` | The app tests (`apptest/`), optionally selected by name |
| `make session-graphs` | Writes both harnesses' session graphs with the reply Trigger binding |
| `node . web`, `node . worker` | The two hosted processes; `node .` with no mode is the local process |
| `make run-eino` | Builds `eino/cmd/signup-eino` into `.local/bin/` and runs it with `.env`: the Eino harness's local process (Worker, watcher and reply Trigger) |
| `make test-eino [RUN=<regexp>]` | The Eino harness's tests (`eino/`): unit tests, the scenario suite and the crash tests. `DEXCLI` names the `dexcli` (1.47.9 or later) that starts their private engines, with the `dex-server-zd` beside it; `EINO_DEX_SERVER_ZD_BINARY` names another one. A server older than server-zd 0.1.34 refuses finish_review's graceful completion, and the tests that end a review say so |

`make bootstrap`, `make check-fdg-v2` and `make check-static` need Go, the version of
[`tools/appmanifest/go.mod`](../tools/appmanifest/go.mod) or later. Python has no YAML parser, so
`scripts/render-flow-definitions.py` reads `dex-app.yaml` only through `tools/appmanifest`, the platform's own
decoder: `go run -C tools/appmanifest . ../../dex-app.yaml` prints the canonical JSON the platform reads. `npm start`,
`npm test`, `make test-app` and `make session-graphs` do not read the manifest and need no Go. `make run-eino` and
`make test-eino` need Go 1.26 or later.

## Files

| Path | Contents |
| --- | --- |
| [`src/signup-agent.ts`](../src/signup-agent.ts) | The prompt, the tools, and the connector tool policies |
| [`src/review-state.ts`](../src/review-state.ts) | The session's records: the review's thread, the latest draft, earlier sends and decisions; the thread index |
| [`src/review-replies.ts`](../src/review-replies.ts) | The reply Trigger's target and runner |
| [`src/signup-watcher.ts`](../src/signup-watcher.ts) | Starts each pending sign-up's session |
| [`src/pi-agent.ts`](../src/pi-agent.ts), [`src/claude-agent.ts`](../src/claude-agent.ts) | The pi and Claude Agent SDK harnesses |
| [`eino/`](../eino) | The Eino harness, a Go module: `cmd/signup-eino` (the local process), `internal/signup` (the port of `src/`, file by file, with the prompt copy `prompt.txt`), `internal/signuptest` (test support) and `scenarios` (the scenario suite and the crash tests) |
| [`src/application.ts`](../src/application.ts) | `loadApplication`: the configuration, services and each mode's background work |
| [`src/config.ts`](../src/config.ts) | The configuration and the pi model rules |
| [`src/review-api.ts`](../src/review-api.ts) | The Dex Cloud ReviewAPI client, including the optional sign-up answers `jobTitle`, `company` and `purpose` |
| [`src/profile-sources.ts`](../src/profile-sources.ts) | `fetch_profile` and `github_user` |
| [`src/main.ts`](../src/main.ts), [`src/runtime.ts`](../src/runtime.ts), [`src/http.ts`](../src/http.ts), [`src/dex.ts`](../src/dex.ts) | The process entry, the hosted runtime settings, the HTTP server and the Dex connection |
| [`flow-definitions/`](../flow-definitions) | The committed session graphs of both harnesses |
| [`scripts/`](../scripts) | `bootstrap.sh`, `render-flow-definitions.py` (behind `check-fdg-v2`) and the binding's Setup units |
| [`dex-app.yaml`](../dex-app.yaml), [`.dex/template.json`](../.dex/template.json), [`openapi/openapi.yaml`](../openapi/openapi.yaml) | The Dex Cloud application manifest (strict YAML, see [dex-cloud.md](dex-cloud.md#connect-a-repository)), template and HTTP API |
| [`tools/appmanifest/`](../tools/appmanifest) | The platform's `dex-app.yaml` decoder (MIT, a Go module of its own), copied unchanged from the Dex Cloud TypeScript template; do not edit it |
| [`apptest/`](../apptest), [`test/`](../test) | The app tests and the scenario and unit tests |

## Tests

No test calls a real model, Slack, Gmail or GitHub.

**App tests** (`make test-app`, [`apptest/`](../apptest)): each test starts its own `dex-server-zd dev` (from
`DEX_SERVER_ZD_BINARY`, by default the one on `PATH`) and runs the hosted pi session Flow on a pull Worker with the
application's watcher and reply Trigger, and the HTTP API in front of it. They cover a review approved and sent
through Slack replies, only a reviewer's replies in review threads being delivered, and the health route.

**Scenario suite** (`npm test`, [`test/`](../test)): the review end to end, once per harness, on a private local Dex
engine started by the test (`dexcli` and `dex-server-zd` must be on `PATH`), with a scripted model, the Slack and
Gmail connector mocks and a test-only ReviewAPI stub. pi gets a scripted model client; the Claude Agent SDK runs
the real bundled Claude Code CLI against a scripted local Messages API, with a dummy key and no network, and the
test checks that the CLI offers only the agent's own tools. Reviewer replies arrive as Slack Events API envelopes on
a Socket Mode stand-in (the connector's public `socketDialer` and `send` options), through the application's own
reply runner, durable inbox and target.

Scenarios:

- A review with two revisions, the approval and the email; each draft's footer follows the account's status, and
  the tools confirm the approval and the send in the thread.
- A bystander's reply, this app's own post, another app's bot message, a bot-message subtype, an edit and a reply in
  another channel are never delivered; each reviewer reply reaches the session once; finish_review ends the session,
  which closes `COMPLETED` with finish_review's result and the final assistant message stored, and a reply after it
  is consumed as undeliverable. Every scenario that reaches finish_review checks the same, and so does the app test
  `TestReviewIsApprovedAndSentFromSlackReplies` (graceful completion needs `dex-server-zd` 0.1.34 or newer on
  `PATH`).
- The same Slack event sent twice and handled once; a reviewer's reply in a thread that is no review's, dropped.
- An approval and a rejection the server refuses, each posting its failure; a rejection with its reason in the
  thread.
- An approval whose answer was lost, tried again and counted as approved; an applied approval whose answer was lost
  while the ReviewAPI stays busy, posted as an unknown outcome, after which `send` still goes out.
- An approval and a send each followed by a failed model call and the reviewer asking again, which repeat neither
  the decision, its confirmation nor the email; an approval asked again after an admin rejected the account, which
  reports the change and posts nothing.
- Time travel of the session to before the send, which sends it again; an approval note whose outcome is unknown
  and one Slack rejects, which leave the approval as it is and report `unknown` and `notPosted`.
- A send whose outcome is unknown, whose one note from the agent points to Sent Mail; a send Gmail rejects,
  recorded as failed with "Nothing was sent". send_welcome_email posts nothing itself.
- Bounded resends: a review post, a draft and a send whose first attempt asks to retry after an unknown outcome,
  each sent again by the tool Step's retry under the same call identity and recorded once, with one note of the
  send; a draft whose outcome stays unknown, which is not the latest draft and may be posted again; a send whose
  outcome stays unknown, after which `send` is refused with "may already have been sent". The mocks keep no fence,
  so these show the Step retry; the fence's read-back and resend decisions are unit tests with the real connectors.
- A draft the same as the latest one, refused; a send before the approval, refused; two tool calls in one model
  response; a sign-up from before `SIGNUP_REVIEW_SINCE`.
- A restart: a new runner with an empty thread index, and a reply that arrives while the Worker is down, retried from
  the inbox and delivered once the Worker is back.

**Eino tests** (`make test-eino`, [`eino/`](../eino)): no real model, Slack or Gmail either.

- Unit tests (`eino/internal/signup`) port `test/review-state.test.ts`, `test/review-replies.test.ts`,
  `test/profile-sources.test.ts`, `test/signup-agent.test.ts`, `test/signup-answers.test.ts` and
  `test/connector-fences.test.ts` (the released Go Slack and Gmail operations with the real fence over a fake
  transport and sdkgo's fake Step context), plus the Eino model rules and endpoints, the connector tools over the
  connector mocks, and a test that fails when `prompt.txt` differs from `signupAgentPrompt` in `src/signup-agent.ts`.
- The scenario suite (`eino/scenarios`) ports `test/support/signup-review-scenarios.ts` scenario by scenario, with
  the same names and assertions, on a private Dex engine: a scripted Eino `ToolCallingChatModel` over the same
  `decideNextStep`, the Go Slack and Gmail mocks, the ReviewAPI stub, and reviewer replies as Slack Events API
  envelopes on a Socket Mode stand-in (a local WebSocket server that `apps.connections.open` points to) through the
  application's own reply runner, durable inbox and target.
- A test of the local process (`LoadApplication`, the Worker, and `Start`'s watcher and reply Trigger) reviews,
  approves and sends a sign-up from Slack replies on the real Slack and Gmail Go connectors against local provider
  stand-ins, as the app test `TestReviewIsApprovedAndSentFromSlackReplies` does for pi.
- Crash tests run the Worker as a child process on the real Slack and Gmail Go connectors against local provider
  stand-ins and SIGKILL it during a `RunModel`, after post_review's Slack post, and during send_welcome_email's Gmail
  send. After the restart the review is posted once, the email is sent once, and the session completes.

**Unit tests** (`test/`): the reply target's routing (filtered, delivered, dropped, retried for a recent thread,
dropped for a disputed thread); the review records read from tool results and never from a turn note; the index
refusing a thread two accounts claim; the optional sign-up answers (`jobTitle`, `company`, `purpose`) read when
present, absent or empty, given to the agent only when present, and shown escaped and cut in the review post; the connector tools' session policies, which set no send cap, and each tool call pinning the connectors' declared cap of 3; a decision note told apart as posted, not posted or unknown; the released Slack postThreadReply and Gmail sendMessage with the real SDK fence over a fake transport and a fake tool Step (`test/connector-fences.test.ts`): the cap of 3 sends, Slack's read-back of a checkpointed `ts`, Gmail's Sent Mail read-back by Message-ID, a definite failure never resent, and a second fenced write in one Step taking over its checkpoint; a send of a draft told apart as sent or possibly sent; the
pi model rules and custom endpoints; and `fetch_profile`'s checks for private addresses, redirects to private
addresses and linear-time parsing of adversarial HTML.
