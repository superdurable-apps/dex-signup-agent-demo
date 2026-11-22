package signup

// The Eino harness: an Eino ADK ChatModelAgent on the Dex Eino adapter runs the session. Every model call is one
// RunModel Step and every tool call its own tool Step, as with the pi harness: the adapter runs the agent in Step mode,
// one model call per Step, and defers each tool call to its tool Step.

import (
	"context"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	gmail "github.com/superdurable/dex-connectors-library/connectors/google/gmail/go"
	slack "github.com/superdurable/dex-connectors-library/connectors/slack/go"
	dexagents "github.com/superdurable/dex/agents/go/core"
	dexeino "github.com/superdurable/dex/agents/go/eino"
)

// EinoHarnessLabel names the harness in the review post.
const EinoHarnessLabel = "eino (Dex Eino adapter, ADK ChatModelAgent)"

// EinoSessionFlowType is the session Flow type of the Eino harness. Never rename it while reviews are open.
const EinoSessionFlowType = "SignupReviewEinoAgentFlow"

// signupToolCallOf names the session and the call of a tool Step from its context.
func signupToolCallOf(ctx context.Context) SignupToolCall {
	call, ok := dexeino.ToolCallContextFrom(ctx)
	if !ok {
		return SignupToolCall{}
	}
	return SignupToolCall{SessionID: call.FlowID, CallKey: call.IdempotencyKey}
}

// NewEinoSignupAgent is the session Flow of the Eino harness over the model, with the prompt, the three plain tools as
// Eino tools, and the six connector tools from the Go connector tool source.
func NewEinoSignupAgent(services *Services, slackConnection slack.Connection, gmailConnection gmail.Connection, chatModel model.ToolCallingChatModel) (*dexagents.AgentSessionFlow, error) {
	tools, err := EinoSignupTools(services, signupToolCallOf)
	if err != nil {
		return nil, err
	}
	source, err := SignupConnectorToolSource(services, slackConnection, gmailConnection)
	if err != nil {
		return nil, err
	}
	// The plain tools' effects, and no Dex approval for any tool: the reviewers approve in Slack.
	policies := SignupConnectorToolPolicies()
	for _, tool := range SignupTools(services) {
		requiresApproval := false
		policies[tool.Name] = dexagents.ToolPolicy{Effect: tool.Effect, RequiresApproval: &requiresApproval}
	}
	return dexeino.NewAgentSessionFlow(signupAgentConfig(chatModel, tools), EinoSessionFlowType, dexeino.WithToolPolicies(policies), dexeino.WithToolSources(source))
}

// einoMaxModelCallsPerTurn is the most model calls of one turn, the pi harness's limit (the Dex built-in loop's
// default). Eino's own default MaxIterations is 20; the adapter ends a turn that reaches it as failed.
const einoMaxModelCallsPerTurn = 100

// signupAgentConfig is the ChatModelAgent of the session: the prompt, the model, the plain tools and the turn limit.
func signupAgentConfig(chatModel model.ToolCallingChatModel, tools []tool.BaseTool) *adk.ChatModelAgentConfig {
	return &adk.ChatModelAgentConfig{
		Name:          "signup-review",
		Description:   "Reviews Dex Cloud sign-ups with the admins in Slack.",
		Instruction:   SignupAgentPrompt,
		Model:         chatModel,
		ToolsConfig:   adk.ToolsConfig{ToolsNodeConfig: compose.ToolsNodeConfig{Tools: tools}},
		MaxIterations: einoMaxModelCallsPerTurn,
	}
}
