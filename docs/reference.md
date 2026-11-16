# Reference

Configuration, commands, files and tests. For the overview, see the [README](../README.md).

## Configuration

Locally, `npm start` loads `.env` (copy [`.env.example`](../.env.example)). Hosted, the application environment
comes from the Project configuration; see [superverse.md](superverse.md#setup). [`src/config.ts`](../src/config.ts)
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
| `SIGNUP_AGENT_HARNESS` | `pi` (default) or `claude-agent-sdk`. Hosted, only `pi` |
| `SIGNUP_AGENT_PI_MODEL` | pi: the model; see [pi model selection](#pi-model-selection). Default `anthropic/claude-opus-5-5` |
| `ANTHROPIC_API_KEY` | pi: required for an `anthropic/…` catalog model. Claude Agent SDK: used with `SIGNUP_AGENT_CLAUDE_AUTH=api-key` |
| `SIGNUP_AGENT_PI_BASE_URL`, `SIGNUP_AGENT_PI_API_KEY` | pi with a custom endpoint: the base URL selects it, and its key is then required |
| `SIGNUP_AGENT_PI_PROTOCOL` | Optional, only with `SIGNUP_AGENT_PI_BASE_URL`: `openai` (default, Chat Completions) or `anthropic` (Messages) |
| `SIGNUP_AGENT_CLAUDE_MODEL` | Claude Agent SDK: the model, `claude-opus-5-5` by default |
| `SIGNUP_AGENT_CLAUDE_AUTH` | Claude Agent SDK: `login` (default, your Claude Code login) or `api-key` |
| `SIGNUP_AGENT_CLAUDE_CLI` | Claude Agent SDK: optional path of a Claude Code CLI to run instead of the bundled one |
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

## Commands

| Command | What it does |
| --- | --- |
| `npm ci` | Installs the pinned dependencies |
| `npm start` | Builds and runs the local process (Worker, watcher and reply Trigger) with `.env` |
| `npm run build` | Compiles `src/` to `dist/` |
| `npm run typecheck` | Type-checks sources and tests |
| `npm test` / `make test` | The scenario suite of both harnesses (`test/`) |
| `make bootstrap` | Restores `node_modules` from `package-lock.json` (`npm ci --ignore-scripts`), only when the lockfile changed |
| `make check-fdg-v2` | Checks the committed session graph and the connector entries of `dex-app.yaml` (see [superverse.md](superverse.md#session-graphs)) |
| `make check-static` | `check-fdg-v2`, then the type check and the build |
| `make test-app [RUN=<regexp>]` | The app tests (`apptest/`), optionally selected by name |
| `make session-graphs` | Writes both harnesses' session graphs with the reply Trigger binding |
| `node . web`, `node . worker` | The two hosted processes; `node .` with no mode is the local process |

## Files

| Path | Contents |
| --- | --- |
| [`src/signup-agent.ts`](../src/signup-agent.ts) | The prompt, the tools, and the connector tool policies |
| [`src/review-state.ts`](../src/review-state.ts) | The session's records: the review's thread, the latest draft, earlier sends and decisions; the thread index |
| [`src/review-replies.ts`](../src/review-replies.ts) | The reply Trigger's target and runner |
| [`src/signup-watcher.ts`](../src/signup-watcher.ts) | Starts each pending sign-up's session |
| [`src/pi-agent.ts`](../src/pi-agent.ts), [`src/claude-agent.ts`](../src/claude-agent.ts) | The two harnesses |
| [`src/application.ts`](../src/application.ts) | `loadApplication`: the configuration, services and each mode's background work |
| [`src/config.ts`](../src/config.ts) | The configuration and the pi model rules |
| [`src/review-api.ts`](../src/review-api.ts) | The Dex Cloud ReviewAPI client |
| [`src/profile-sources.ts`](../src/profile-sources.ts) | `fetch_profile` and `github_user` |
| [`src/main.ts`](../src/main.ts), [`src/runtime.ts`](../src/runtime.ts), [`src/http.ts`](../src/http.ts), [`src/dex.ts`](../src/dex.ts) | The process entry, the hosted runtime settings, the HTTP server and the Dex connection |
| [`flow-definitions/`](../flow-definitions) | The committed session graphs of both harnesses |
| [`scripts/`](../scripts) | `bootstrap.sh`, `render-flow-definitions.py` (behind `check-fdg-v2`) and the binding's Setup units |
| [`dex-app.yaml`](../dex-app.yaml), [`.superverse/template.json`](../.superverse/template.json), [`openapi/openapi.yaml`](../openapi/openapi.yaml) | The Superverse application manifest, template and HTTP API |
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
  and a reply after it is consumed as undeliverable.
- The same Slack event sent twice and handled once; a reviewer's reply in a thread that is no review's, dropped.
- An approval and a rejection the server refuses, each posting its failure; a rejection with its reason in the
  thread.
- An approval whose answer was lost, tried again and counted as approved; an applied approval whose answer was lost
  while the ReviewAPI stays busy, posted as an unknown outcome, after which `send` still goes out.
- An approval and a send each followed by a failed model call and the reviewer asking again, which repeat neither
  the decision, its confirmation nor the email; an approval asked again after an admin rejected the account, which
  reports the change and posts nothing.
- Time travel of the session to before the send, which sends it again; a confirmation post that fails without
  changing the approval.
- A send whose outcome is unknown, with the Sent Mail note; a send Gmail rejects, recorded as failed with
  "Nothing was sent".
- A draft the same as the latest one, refused; a send before the approval, refused; two tool calls in one model
  response; a sign-up from before `SIGNUP_REVIEW_SINCE`.
- A restart: a new runner with an empty thread index, and a reply that arrives while the Worker is down, retried from
  the inbox and delivered once the Worker is back.

**Unit tests** (`test/`): the reply target's routing (filtered, delivered, dropped, retried for a recent thread,
dropped for a disputed thread); the review records read from tool results and never from a turn note; the index
refusing a thread two accounts claim; the send cap of send_welcome_email's outcome reply against the SDK fence; the
pi model rules and custom endpoints; and `fetch_profile`'s checks for private addresses, redirects to private
addresses and linear-time parsing of adversarial HTML.
