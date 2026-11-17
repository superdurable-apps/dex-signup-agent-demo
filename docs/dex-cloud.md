# Run it on Dex Cloud

How to deploy and run the review agent on Dex Cloud. For the design, see [design.md](design.md); for all settings
and commands, [reference.md](reference.md).

The hosted path is tested only with the connector's local stand-ins and the app tests' Socket Mode stand-in; it has
not been verified against a live relay.

## Connect a repository

The repository is a Dex Cloud application of the TypeScript base. Connect it as an application; Dex Cloud reads:

- [`.dex/template.json`](../.dex/template.json): build profile `node-ts-v1`, kind `agent`, the agent
  instructions [`AGENTS.md`](../AGENTS.md), the API [`openapi/openapi.yaml`](../openapi/openapi.yaml), and the
  commands `make bootstrap`, `make check-static` and `make test-app`.
- [`dex-app.yaml`](../dex-app.yaml): the application (port 8080, health path `/api/health`, 16 Worker slots, the
  environment it accepts), the committed Flow graph, and the two connections.

`dex-app.yaml` is one strict YAML document: no repeated key, anchor, alias, merge key or tag, no signed or
floating-point number and no date, and every plain value that YAML 1.1 and YAML 1.2 read differently is quoted, such
as a dotted version (`version: "0.7.0"`), `yes`, `no`, `on`, `off` or a leading zero. JSON is accepted too.
[`tools/appmanifest`](../tools/appmanifest), the platform's own decoder, applies these rules; `make check-fdg-v2`
reads the manifest through it.

## Setup

### Connections

`dex-app.yaml` names each connector by npm `package` and exact `version`, the versions `package.json` pins
(`make check-fdg-v2` fails when they differ):

| Connection | Connector | Package | `authMethodId` | Operations |
| --- | --- | --- | --- | --- |
| `review-slack` | `slack` | `@superdurable/connector-slack` 0.8.0 | `""` | `postChannelMessage`, `postThreadReply`; Trigger `threadReplyCreated`, binding `signup-review-reply` |
| `welcome-gmail` | `gmail` | `@superdurable/connector-google-gmail` 0.4.0 | `google-oauth` | `sendMessage` |

- `review-slack` is the Project's shared Slack connection. It has no app-level token, so replies arrive through the
  platform's relay, not Socket Mode.
- `welcome-gmail` is a user-owned Google OAuth client connection of the mailbox that sends the emails; there is no
  platform-shared Google client.

### The reply Trigger binding

Fill in the `signup-review-reply` binding in Studio Setup. Its units come from
[`scripts/session-trigger-bindings.json`](../scripts/session-trigger-bindings.json):

| Unit | Label | Required | Stored at |
| --- | --- | --- | --- |
| `channelPicker` | Review channel | yes | `/channelId` |
| `memberPicker` | Reviewers (at least one) | yes | `/threadReplyMatcher/posterUserIds` |
| `textInput` | Reviewer reply contains | no | `/threadReplyMatcher/messageContains` |

Hosted, the binding is the only source of the review channel and the reviewers: `SLACK_REVIEW_CHANNEL` and
`SIGNUP_REVIEWER_SLACK_IDS` are not read, and post_review posts in the binding's channel. The connector reads the
binding at start, so a change takes effect when the processes restart.

### Environment and secrets

Set the application environment in Setup. `dex-app.yaml` declares:

| Variable | Kind |
| --- | --- |
| `REVIEW_API_URL` | Required |
| `REVIEW_API_KEY` | Required, secret |
| `SIGNUP_REVIEW_SINCE` | Required, an RFC 3339 time |
| `ANTHROPIC_API_KEY` | Secret, optional in the manifest |
| `SIGNUP_AGENT_HARNESS` | Optional, only `pi` |
| `SIGNUP_AGENT_PI_MODEL`, `SIGNUP_AGENT_PI_BASE_URL` | Optional |
| `SIGNUP_AGENT_PI_PROTOCOL` | Optional choice: `openai` or `anthropic` |
| `SIGNUP_AGENT_PI_API_KEY` | Secret, optional in the manifest |
| `SIGNUP_WATCH_INTERVAL_SECONDS` | Optional |
| `GITHUB_TOKEN` | Secret, optional |

The model settings are all optional in the manifest, because which ones are needed depends on the model, and the
manifest has no way to say that. Use one of these combinations:

| Model | Set |
| --- | --- |
| Anthropic, the default `anthropic/claude-opus-5-5` | secret `ANTHROPIC_API_KEY`; optionally `SIGNUP_AGENT_PI_MODEL` with another `anthropic/…` model |
| OpenAI-compatible Chat Completions endpoint | `SIGNUP_AGENT_PI_BASE_URL`, `SIGNUP_AGENT_PI_MODEL` (the endpoint's model ID), secret `SIGNUP_AGENT_PI_API_KEY`; leave `SIGNUP_AGENT_PI_PROTOCOL` unset or `openai` |
| Anthropic-compatible Messages endpoint | the same, with `SIGNUP_AGENT_PI_PROTOCOL` = `anthropic` |

Other catalog providers are not available hosted: their key variables are not declared. The processes check the
combination at start and refuse to start with a message that says what to set; the full rules are in
[pi model selection](reference.md#pi-model-selection). The processes apply the environment to a copy of their own,
never to `process.env`.

The platform itself sets `PORT` (web only), `DEX_FLOW_SERVICE_ADDRESS`, `DEX_TOKEN_URL`, `DEX_BLOB_CACHE_DIR`,
`DEX_PROJECT_CONFIG_KEY` and, on Live, `DEX_API_KEY` and `DEX_SERVER_ID`.

## Build and processes

Dex Cloud builds the image with `npm ci --ignore-scripts` and `npm run build`, without network, on Node.js 22, and
runs two processes from it:

- **`node . web`** serves `GET /api/health` (the manifest's `healthPath`) and the relayed Slack events at
  `/triggers/slack/cmV2aWV3LXNsYWNr` (the connector's relay path for `review-slack`) on `PORT`, and registers that
  route at the platform. It runs the reply Trigger. A relayed event that arrives before the Trigger runs gets a 503.
- **`node . worker`** reads `/tmp/superverse-app/runtime.yaml` (Worker slots, which come from
  `application.workerSlots` = 16, and the drain timeout), answers `/readyz` once its slots are registered, and runs
  the Dex Worker and the watcher. It uploads no Flow definition; the platform publishes the committed graph.

Both write only under `/tmp` and stop on SIGTERM; the Worker drains its running tasks first.

The platform routes Slack events by workspace, not by channel, so the binding's channel and the target's filter
decide what is delivered. It retries an event the web process does not accept, which also covers the short window
of a web rollout without a receiver. The web process verifies each `Dex-Relay-Signature` and keeps the Trigger's
inbox in the Project's storage. Unlike Socket Mode, the relay sends each registered deployment its own copy of the
events.

## Preview and Live

Preview and Live build from this repository and run the same two processes. The differences the code sees:

- Live sets `DEX_API_KEY` and `DEX_SERVER_ID` (and, where needed, `DEX_TOKEN_CA_FILE`) for the token exchange;
  Preview passes no key.
- `PORT` is 8080 on Live and 3000 on Preview.

## Session graphs

Only the pi session Flow, `SignupReviewPiAgentFlow`, runs hosted. Its graph is committed in
[`flow-definitions/signup-review-pi-agent.json`](../flow-definitions/signup-review-pi-agent.json), written by the
installed `@superdurable/dex-agents`. Never edit a graph by hand.

Setup accepts the `signup-review-reply` binding of `dex-app.yaml` only because the graph declares it in
`v2.connectorTriggerBindings`, with the Setup units of
[`scripts/session-trigger-bindings.json`](../scripts/session-trigger-bindings.json).

Write both harnesses' graphs again with `make session-graphs` after a change to the binding, the Slack connector
version, the Flow type or the permission, or a `@superdurable/dex-agents` update. For each harness it runs:

```sh
npx --no-install dex-agents-session-graph --flow-type <type> --permission signup-review.operate \
  --trigger-bindings scripts/session-trigger-bindings.json --out flow-definitions/<graph>.json
```

`make check-fdg-v2` fails while a graph differs from that output, or while its binding (connector, connection,
Trigger, binding name, package and version) differs from `dex-app.yaml`.

Locally, the Worker uploads its harness's graph from `flow-definitions/` at start, but the Server keeps a stored
graph unless the new one was generated later, and a session graph keeps its template's generation time. To replace
a stored graph without the binding, upload once with `Client.uploadFlowDefinitions` and `force`.

## Constraints

- Only the pi harness runs hosted. `SIGNUP_AGENT_HARNESS` other than `pi` refuses to start there; the Claude Agent
  SDK harness runs only locally.
- Egress is public HTTPS on port 443 only: `REVIEW_API_URL` and the pages `fetch_profile` reads must be on 443
  (`fetch_profile` refuses other ports anyway).
- A reply event the relay cannot deliver to the web process before its retries run out is lost; the admin replies
  again.
- The review channel and the reviewers are read from the binding at start: change them in Setup, then restart.
- Never rename the Flow type of a session with open reviews.
