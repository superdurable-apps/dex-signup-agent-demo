// The application, as each process loads it: the review session Flow and its services, and the background work of the
// process's mode. Hosted, `web` runs the reply Trigger behind the HTTP server and `worker` runs the Dex Worker and the
// watcher; the local process and the app tests run all of it.

import { newProjectConnection as newGmailConnection, type GmailConnection } from "@superdurable/connector-google-gmail";
import { newProjectConnection as newSlackConnection, type Connection as SlackConnection, type MessageTriggerRunner, type SlackClientOptions } from "@superdurable/connector-slack";
import { TriggerDeliveryAbortedError, newTriggerContext, type TriggerLogger } from "@superdurable/connectors-sdk";
import { loadFromEnvironment, type LoadedProject } from "@superdurable/connectors-sdk/projectconfig";
import type { ModelClient } from "@superdurable/dex-agents/builtin-loop";
import type { Client, Flow } from "@superdurable/dex";

import { claudeCliEnvironment, claudeHarnessLabel, createClaudeSignupAgent } from "./claude-agent.ts";
import { loadSignupAgentConfig, type SignupAgentConfig } from "./config.ts";
import type { Route } from "./http.ts";
import { configuredPiModel, createPiSignupAgent, piHarnessLabel } from "./pi-agent.ts";
import { PublicProfileSources, type ProfileSources } from "./profile-sources.ts";
import { ReviewApiClient } from "./review-api.ts";
import { createReviewReplyRunner, createReviewReplyTarget, reviewChannelOf, reviewSlackConnection } from "./review-replies.ts";
import { ReviewSessions, type ReviewSessionFlow } from "./review-state.ts";
import type { SignupServices } from "./signup-agent.ts";
import { runSignupWatcher } from "./signup-watcher.ts";

/** The process the application runs in: hosted `web` or `worker`, the local process, or an app test. */
export type ApplicationMode = "web" | "worker" | "local" | "test";

/** What the application loads with: the mode, and in tests what they replace. */
export interface ApplicationOptions {
  readonly mode: ApplicationMode;
  /** The environment to read instead of the process's. */
  readonly environment?: NodeJS.ProcessEnv;
  /** The connector project instead of the one loadFromEnvironment selects. */
  readonly project?: LoadedProject;
  /** The tools' Slack and Gmail connections, such as connector mocks. */
  readonly slack?: SlackConnection;
  readonly gmail?: GmailConnection;
  readonly profiles?: ProfileSources;
  /** The pi model instead of the configured one, such as a scripted model. */
  readonly model?: ModelClient;
  /** The reply runner's Slack client options, such as a Socket Mode stand-in, and its Trigger logger. */
  readonly slackOptions?: SlackClientOptions;
  readonly triggerLogger?: TriggerLogger;
}

/** The background work of one process; `done` rejects when the reply Trigger fails for good. */
export interface RunningApplication {
  readonly replies: MessageTriggerRunner | undefined;
  readonly done: Promise<void>;
  stop(): Promise<void>;
}

/** The application one process loads. */
export interface Application {
  /** The Flows the Worker registers: the session Flow of the configured harness, and nothing else. */
  readonly flows: readonly Flow<any>[];
  readonly sessionFlow: ReviewSessionFlow;
  readonly services: SignupServices;
  readonly config: SignupAgentConfig;
  /** Whether the process runs on Superverse, selected by the platform's DEX_PROJECT_CONFIG_KEY. */
  readonly isHosted: boolean;
  /** The API of openapi/openapi.yaml beyond GET /api/health; the review happens in Slack, so there is none. */
  routes(client: Client): Route[];
  readonly page: string;
  /**
   * Gives the tools and the thread index the process's Client. Call it right after the Client is created, before the
   * Worker starts: a review whose turn was in flight at a restart runs its tool Steps as soon as the Worker pulls.
   */
  connect(client: Client): void;
  /** Starts the mode's background work over the connected Client: the watcher (all but web) and the reply Trigger (all but worker). */
  start(client: Client): RunningApplication;
}

const page = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Dex signup review agent</title></head>
<body><h1>Dex signup review agent</h1><p>Reviews of Dex Cloud sign-ups happen in Slack. Each review's agent session is <code>signup-review-&lt;accountId&gt;</code> in Studio and Dex Web.</p></body></html>
`;

/**
 * Loads the configuration and builds the session Flow and its services. Hosted, the application environment comes
 * from the Project configuration (into a copy of the process environment, so no secret enters process.env), and the
 * review channel and reviewers from the Project's reply binding.
 */
export async function loadApplication(options: ApplicationOptions): Promise<Application> {
  const environment: NodeJS.ProcessEnv = { ...(options.environment ?? process.env) };
  const project = options.project ?? (await loadFromEnvironment(environment));
  const isHosted = options.mode !== "test" && Boolean(environment.DEX_PROJECT_CONFIG_KEY);
  if (isHosted) {
    (await project.resolveApplicationEnvironment()).apply(environment);
  }
  const config = loadSignupAgentConfig(environment, isHosted ? reviewChannelOf(project) : undefined);
  const services: SignupServices = {
    sessions: new ReviewSessions(),
    reviewApi: new ReviewApiClient({ baseUrl: config.reviewApiUrl, apiKey: config.reviewApiKey }),
    profiles: options.profiles ?? new PublicProfileSources(config.githubToken === undefined ? {} : { githubToken: config.githubToken }),
    slack: options.slack ?? newSlackConnection(project, reviewSlackConnection),
    gmail: options.gmail ?? newGmailConnection(project, "welcome-gmail"),
    slackChannelId: config.channel.channelId,
    harnessLabel: config.harness === "claude-agent-sdk" ? claudeHarnessLabel : piHarnessLabel,
  };
  // SIGNUP_AGENT_HARNESS picks the harness; the prompt, the tools and the reply Trigger are the same for both.
  const sessionFlow =
    config.harness === "claude-agent-sdk"
      ? createClaudeSignupAgent(services, {
          model: config.claudeModel,
          cliEnvironment: claudeCliEnvironment(config.claudeAuthentication, environment),
          ...(config.claudeCliPath === undefined ? {} : { cliPath: config.claudeCliPath }),
        })
      : createPiSignupAgent(services, options.model ?? configuredPiModel(config.piModel!));
  return {
    flows: [sessionFlow],
    sessionFlow,
    services,
    config,
    isHosted,
    routes: () => [],
    page,
    connect(client) {
      services.sessions.connect(client, sessionFlow);
    },
    start(client) {
      const stopping = new AbortController();
      const tasks: Promise<void>[] = [];
      if (options.mode !== "web") {
        tasks.push(runSignupWatcher({ client, sessionFlow, reviewApi: services.reviewApi, reviewSince: config.reviewSince }, config.watchIntervalMs, stopping.signal));
      }
      let replies: MessageTriggerRunner | undefined;
      if (options.mode !== "worker") {
        const target = createReviewReplyTarget({ client, sessionFlow, sessions: services.sessions, channel: config.channel, ...(options.triggerLogger === undefined ? {} : { logger: options.triggerLogger }) });
        // Hosted, the binding comes from the Project and the events from the platform's relay; locally, the binding
        // comes from the environment and the events over Socket Mode, with the connection's app_token.
        replies = createReviewReplyRunner(project, isHosted ? "project" : config.channel, target, { ...options.slackOptions, ...(options.triggerLogger === undefined ? {} : { logger: options.triggerLogger }) });
        tasks.push(
          replies.run(newTriggerContext(stopping.signal)).catch((error: unknown) => {
            if (!(error instanceof TriggerDeliveryAbortedError && stopping.signal.aborted)) {
              throw new Error(`the Slack reply Trigger stopped: ${error instanceof Error ? error.message : String(error)}`, { cause: error });
            }
          }),
        );
      }
      const done = Promise.all(tasks).then(() => undefined);
      return {
        replies,
        done,
        async stop() {
          stopping.abort();
          await Promise.allSettled(tasks);
        },
      };
    },
  };
}
