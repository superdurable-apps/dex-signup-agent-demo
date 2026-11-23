# Usage operations

The onboarding wizard reads each account's usage through two read-only operations of the Dex Cloud account review
API: the same base URL, keys, JSON conventions and error codes as `list-pending`, `get`, `approve` and `reject`.
Dex Cloud does not serve them yet; until it does, [`tools/usage-standin.ts`](../tools/usage-standin.ts) serves this
contract locally. [`src/usage-api.ts`](../src/usage-api.ts) is the client.

## `POST /v1/account-review/get-usage`

```json
{"accountId": "acct_..."}
```

Returns `{"usage": AccountUsage}`, or `404 not_found`. Every time is RFC 3339 in UTC; a milestone is `null` until
the account reaches it. "Activity" is a sign-in, a message to the Studio agent, a Preview start or a publish by the
account. Counts and projects cover the TeamSpaces the account belongs to, so a teammate's work counts too:
onboarding is about whether the team gets going.

| Member | Meaning |
| --- | --- |
| `accountId`, `status` | `active`, `disabled` or `deleted` |
| `asOf` | When the server read the usage |
| `approvedAt` | When the account was approved |
| `lastSignInAt`, `lastActivityAt` | The latest sign-in, and the latest activity |
| `activeDays7d`, `activeDays28d` | Days with any activity in the last 7 and 28 days |
| `milestones` | When the account first: signed in (`firstSignInAt`), created a TeamSpace and a project (`teamSpaceCreatedAt`, `projectCreatedAt`), messaged the Studio agent (`firstAgentMessageAt`), linked a repository (`repositoryLinkedAt`), created a connection (`firstConnectionAt`), had a Preview ready (`firstPreviewReadyAt`) and went Live (`firstLiveAt`) |
| `counts` | `teamSpaces`, `projects`, `liveProjects`, `connections`, `members`, `agentMessages7d`, `agentMessages28d`, `previewStarts7d`, `previewFailures7d`, `publishes28d`, `triggerEvents7d` (events the Live applications received) |
| `connectors` | The connector IDs of the active connections, such as `slack` |
| `projects` | At most 20, most recently active first: `name`, `createdAt`, `lastActivityAt`, `stage` (`created`, `building`, `preview`, `live`), and `lastPreviewFailure` (`at`, `message`) when the latest Preview start failed |

## `POST /v1/account-review/list-active`

```json
{"approvedSince": "2026-11-23T00:00:00Z", "pageSize": 100, "pageToken": ""}
```

Returns `{"accounts": [{"accountId", "email", "displayName", "approvedAt"}], "nextPageToken"}`: the active accounts
approved at or after `approvedSince`, oldest approval first, paged like `list-pending`. The watcher uses it only to
start the onboarding of accounts approved without the review's handoff.

## Privacy

The operations return counts, times, connector IDs, project names and Preview failure messages, never message
contents, code or credentials. The agent passes them to the model and to the admins' Slack channel. The follow-up
emails never mention tracking or metrics; the prompt says so.
