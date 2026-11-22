package signup

import (
	"fmt"

	gmail "github.com/superdurable/dex-connectors-library/connectors/google/gmail/go"
	slack "github.com/superdurable/dex-connectors-library/connectors/slack/go"
	"github.com/superdurable/dex/agents/go/core/connectortools"
)

// SignupConnectorToolSource is the Go connector tool source of the six connector tools, with the tools' own names and
// descriptions and no Dex approval: the reviewers approve in Slack, and the tools check it.
func SignupConnectorToolSource(services *Services, slackConnection slack.Connection, gmailConnection gmail.Connection) (*connectortools.ConnectorToolSource, error) {
	agentTools, err := SignupConnectorAgentTools(services, slackConnection, gmailConnection)
	if err != nil {
		return nil, err
	}
	tools := make([]*connectortools.ConnectorTool, 0, len(agentTools))
	for _, agentTool := range agentTools {
		tool, err := connectortools.NewConnectorTool(agentTool.AgentTool, connectortools.WithToolName(agentTool.Name),
			connectortools.WithToolDescription(agentTool.Description), connectortools.WithRequiresApproval(false))
		if err != nil {
			return nil, fmt.Errorf("connector tool %s: %w", agentTool.Name, err)
		}
		tools = append(tools, tool)
	}
	return connectortools.NewConnectorToolSource(tools)
}
