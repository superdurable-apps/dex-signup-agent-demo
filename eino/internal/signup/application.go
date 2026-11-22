package signup

// The application of the local Eino process: the configuration, the session Flow and its services, and the background
// work, as src/application.ts loads them in local mode. One process runs the Dex Worker, the watcher and the reply
// Trigger.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cloudwego/eino/components/model"
	gmail "github.com/superdurable/dex-connectors-library/connectors/google/gmail/go"
	slack "github.com/superdurable/dex-connectors-library/connectors/slack/go"
	"github.com/superdurable/dex-connectors-library/sdkgo/projectconfig"
	dexagents "github.com/superdurable/dex/agents/go/core"
	"github.com/superdurable/dex/sdk-go/dex"
)

// ApplicationOptions are what the application loads with; tests replace parts.
type ApplicationOptions struct {
	// Environment is a copy of the process environment with .env applied.
	Environment map[string]string
	// Project is the connector project instead of DEX_CONNECTOR_CONFIG_FILE's.
	Project *projectconfig.LoadedProject
	// Slack and Gmail are the tools' connections, such as connector mocks, instead of the project's.
	Slack *slack.Connection
	Gmail *gmail.Connection
	// Profiles are the profile sources instead of the public internet.
	Profiles ProfileSources
	// ChatModel is the model instead of the configured endpoint, such as a scripted model.
	ChatModel model.ToolCallingChatModel
	// SlackOptions are the reply runner's Slack client options; TriggerLogger is its logger.
	SlackOptions  []slack.Option
	TriggerLogger *slog.Logger
}

// Application is what the local process runs.
type Application struct {
	Config      Config
	Project     *projectconfig.LoadedProject
	Services    *Services
	SessionFlow *dexagents.AgentSessionFlow
	options     ApplicationOptions
}

// LoadApplication reads the configuration and builds the session Flow and its services. A bad setting stops the start
// with a message that says what to set.
func LoadApplication(ctx context.Context, options ApplicationOptions) (*Application, error) {
	config, err := LoadConfig(options.Environment)
	if err != nil {
		return nil, err
	}
	project := options.Project
	if project == nil {
		if project, err = projectconfig.LoadDexWebConnectionsFile(ctx, config.ConnectorConfigFile); err != nil {
			return nil, fmt.Errorf("DEX_CONNECTOR_CONFIG_FILE: %w", err)
		}
	}
	reviewAPI, err := NewReviewAPIClient(config.ReviewAPIURL, config.ReviewAPIKey)
	if err != nil {
		return nil, err
	}
	services := &Services{
		Sessions: NewReviewSessions(), ReviewAPI: reviewAPI, Profiles: options.Profiles,
		SlackChannelID: config.Channel.ChannelID, HarnessLabel: EinoHarnessLabel,
	}
	if services.Profiles == nil {
		services.Profiles = PublicProfileSources{GitHubToken: config.GitHubToken}
	}
	slackConnection, gmailConnection, err := toolConnections(project, options)
	if err != nil {
		return nil, err
	}
	chatModel := options.ChatModel
	if chatModel == nil {
		if chatModel, err = NewEinoChatModel(ctx, config.Model); err != nil {
			return nil, fmt.Errorf("the Eino model: %w", err)
		}
	}
	flow, err := NewEinoSignupAgent(services, slackConnection, gmailConnection, chatModel)
	if err != nil {
		return nil, err
	}
	return &Application{Config: config, Project: project, Services: services, SessionFlow: flow, options: options}, nil
}

func toolConnections(project *projectconfig.LoadedProject, options ApplicationOptions) (slack.Connection, gmail.Connection, error) {
	var slackConnection slack.Connection
	var gmailConnection gmail.Connection
	var err error
	if options.Slack != nil {
		slackConnection = *options.Slack
	} else if slackConnection, err = slack.NewProjectConnection(project, ReviewSlackConnection); err != nil {
		return slack.Connection{}, gmail.Connection{}, err
	}
	if options.Gmail != nil {
		gmailConnection = *options.Gmail
	} else if gmailConnection, err = gmail.NewProjectConnection(project, WelcomeGmailConnection); err != nil {
		return slack.Connection{}, gmail.Connection{}, err
	}
	return slackConnection, gmailConnection, nil
}

// Connect gives the tools and the thread index the process's Client. Call it right after the Client is created,
// before the Worker starts: a review whose turn was in flight at a restart runs its tool Steps as soon as the Worker
// pulls.
func (application *Application) Connect(client *dex.Client) {
	application.Services.Sessions.Connect(client, application.SessionFlow.GetFlowType(), dexagents.NewAgentSessionClient(client, application.SessionFlow))
}

// RunningApplication is the background work of the process.
type RunningApplication struct {
	// Done receives the reply Trigger's failure, or nil once both stopped.
	Done   <-chan error
	cancel context.CancelFunc
	tasks  <-chan struct{}
}

// Stop ends the background work and waits for it.
func (running *RunningApplication) Stop() {
	running.cancel()
	<-running.tasks
}

// Start runs the watcher and the reply Trigger over the connected Client until Stop.
func (application *Application) Start(ctx context.Context, client *dex.Client) (*RunningApplication, error) {
	logger := application.options.TriggerLogger
	if logger == nil {
		logger = slog.Default()
	}
	target := NewReviewReplyTarget(ReviewReplyParts{
		Client: client, SendUserMessage: application.SessionFlow.SendUserMessage, Sessions: application.Services.Sessions,
		Channel: application.Config.Channel, Logger: logger,
	})
	// Locally the binding comes from the environment and the events over Socket Mode, with the connection's app_token.
	runner, err := NewReviewReplyRunner(application.Project, application.Config.Channel, target, logger, application.options.SlackOptions...)
	if err != nil {
		return nil, fmt.Errorf("the Slack reply Trigger: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	tasks := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		RunSignupWatcher(ctx, SignupWatcherParts{
			Sessions: dexagents.NewAgentSessionClient(client, application.SessionFlow), ReviewAPI: application.Services.ReviewAPI, ReviewSince: application.Config.ReviewSince,
		}, application.Config.WatchInterval)
	}()
	go func() {
		err := runner.Run(ctx)
		if err != nil && !(errors.Is(err, context.Canceled) && ctx.Err() != nil) {
			done <- fmt.Errorf("the Slack reply Trigger stopped: %w", err)
		}
		<-watcherDone
		close(done)
		close(tasks)
	}()
	return &RunningApplication{Done: done, cancel: cancel, tasks: tasks}, nil
}
