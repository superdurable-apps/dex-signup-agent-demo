// TEST ONLY: Slack's side of the reply Trigger. Reviewer replies arrive as Events API envelopes on a Socket Mode
// stand-in, through the connector's public socketDialer and send options, so the application's own runner, durable
// inbox and target handle them. The connector's mocks have no Trigger injection point.

import type { SlackSocketConnection } from "@superdurable/connector-slack";
import { postChannelMessageSent, postThreadReplySent, type GeneratedMock } from "@superdurable/connector-slack/mock";
import type { TriggerLogAttributes, TriggerLogger } from "@superdurable/connectors-sdk";
import { ConfigurationSchemaVersion, ConnectionStore, CredentialMaterial, InMemoryObjectStore, LoadedProject } from "@superdurable/connectors-sdk/projectconfig";
import type { ProviderRequest, ProviderResponse } from "@superdurable/connectors-sdk/providerhttp";

import { reviewSlackConnection } from "../../src/review-replies.ts";

export const reviewChannelId = "CREVIEWS01";
export const appBotUserId = "UBOT0000001";

/** One Socket Mode WebSocket: the envelopes Slack would send, and the acknowledgements the runner writes. */
class FakeSocket implements SlackSocketConnection {
  private readonly inbound: unknown[] = [];
  private readonly acknowledged: string[];
  private wake: (() => void) | undefined;
  public isClosed = false;

  public constructor(acknowledged: string[]) {
    this.acknowledged = acknowledged;
  }

  public push(message: unknown): void {
    this.inbound.push(message);
    this.wake?.();
  }

  public async readMessage(readTimeoutMs: number): Promise<unknown> {
    const deadline = Date.now() + readTimeoutMs;
    for (;;) {
      if (this.inbound.length > 0) {
        return this.inbound.shift();
      }
      if (this.isClosed || Date.now() >= deadline) {
        throw new Error(this.isClosed ? "connection closed" : "read timeout");
      }
      await new Promise<void>((resolve) => {
        const timer = setTimeout(resolve, Math.max(1, deadline - Date.now()));
        this.wake = () => {
          clearTimeout(timer);
          resolve();
        };
      });
      this.wake = undefined;
    }
  }

  public async writeMessage(value: unknown): Promise<void> {
    this.acknowledged.push((value as { envelope_id: string }).envelope_id);
  }

  public close(): void {
    this.isClosed = true;
    this.wake?.();
  }
}

/** Slack's side of Socket Mode: envelopes go to the open connection, or wait for the next one, as Slack holds them. */
export class SlackEvents {
  public readonly acknowledged: string[] = [];
  private socket: FakeSocket | undefined;
  private readonly pending: unknown[] = [];
  private envelopes = 0;
  private events = 0;

  public readonly dial = async (): Promise<SlackSocketConnection> => {
    const socket = new FakeSocket(this.acknowledged);
    for (const envelope of this.pending.splice(0)) {
      socket.push(envelope);
    }
    this.socket = socket;
    return socket;
  };

  /** Sends one message event in the review channel, unless `message` names another; returns its event ID. */
  public push(message: Record<string, unknown>, eventId = `Ev${++this.events}`): string {
    const envelope = {
      envelope_id: `envelope-${++this.envelopes}`,
      type: "events_api",
      payload: { event_id: eventId, event_time: 1760227200, team_id: "T1", event: { type: "message", channel: reviewChannelId, ...message } },
    };
    if (this.socket !== undefined && !this.socket.isClosed) {
      this.socket.push(envelope);
    } else {
      this.pending.push(envelope);
    }
    return eventId;
  }
}

/** A review thread: the tool mocks post its root and replies, and people reply in it through {@link SlackEvents}. */
export class ReviewThread {
  public readonly rootTs: string;
  private readonly events: SlackEvents;
  private seconds: number;

  public constructor(slack: GeneratedMock, events: SlackEvents, sessionId: string, rootTs: string) {
    this.rootTs = rootTs;
    this.events = events;
    this.seconds = Math.floor(Number(rootTs));
    slack.postChannelMessage().forFlow(sessionId).default(postChannelMessageSent({ message: { channelId: reviewChannelId, timestamp: rootTs, userId: appBotUserId, text: "review" } }));
    slack
      .postThreadReply()
      .forFlow(sessionId)
      .default(postThreadReplySent({ message: { channelId: reviewChannelId, timestamp: `${this.seconds}.000900`, threadTimestamp: rootTs, userId: appBotUserId, text: "ok" } }));
  }

  /** A reply in this thread; `extra` adds or overrides event fields, such as `bot_id`. */
  public reply(userId: string, text: string, extra: Record<string, unknown> = {}): { ts: string; eventId: string; message: Record<string, unknown> } {
    this.seconds += 1;
    const ts = `${this.seconds}.000200`;
    const message = { user: userId, text, ts, thread_ts: this.rootTs, ...extra };
    return { ts, eventId: this.events.push(message), message };
  }
}

/** Keeps every Trigger record, so a test can find a skipped event. */
export class RecordingLogger implements TriggerLogger {
  public readonly records: { level: string; message: string; attributes: TriggerLogAttributes }[] = [];

  public isEnabled(): boolean {
    return true;
  }

  public log(level: string, message: string, attributes: TriggerLogAttributes): void {
    this.records.push({ level, message, attributes });
  }

  public find(message: string, attribute: string, value: string) {
    return this.records.filter((record) => record.message === message && record.attributes[attribute] === value);
  }
}

/** A local project whose review-slack connection has an app token, so the reply runner uses Socket Mode. */
export async function socketModeProject(): Promise<LoadedProject> {
  const objects = new InMemoryObjectStore();
  const scope = { projectId: "signup-review-test", kind: "preview" } as const;
  await new ConnectionStore(objects, scope).replaceCredential(
    { connectorId: "slack", connectionName: reviewSlackConnection },
    0,
    new CredentialMaterial({ bot_token: "xoxb-test", user_token: "xoxp-test", app_token: "xapp-test" }, undefined, "oauth2"),
  );
  return new LoadedProject(
    {
      schemaVersion: ConfigurationSchemaVersion,
      scope,
      revision: 1,
      connections: [{ connectorId: "slack", connectionName: reviewSlackConnection, modulePath: "slack", provider: "slack", configuration: { endpoint: "http://127.0.0.1:9/api" } }],
      triggerBindings: [],
      operationConfigurations: [],
    },
    undefined,
    objects,
    undefined,
    undefined,
  );
}

/** Slack's Web API as the Socket Mode runner calls it: apps.connections.open answers with a WebSocket URL. */
export const slackWebApi = async (request: ProviderRequest): Promise<ProviderResponse> => {
  const answer = new URL(request.url).pathname.endsWith("/apps.connections.open") ? { ok: true, url: "wss://wss-primary.slack.com/link/?ticket=test" } : { ok: false, error: "unknown_method" };
  return { status: 200, headers: { "content-type": "application/json" }, body: Buffer.from(JSON.stringify(answer)) };
};

/** The reply runner's Slack options over the stand-in. */
export const socketModeOptions = (events: SlackEvents, logger: TriggerLogger) => ({
  send: slackWebApi,
  socketDialer: events.dial,
  triggerTimings: { reconnectDelayMs: 5, reconnectMaxDelayMs: 20, readTimeoutMs: 10 * 60_000 },
  logger,
});
