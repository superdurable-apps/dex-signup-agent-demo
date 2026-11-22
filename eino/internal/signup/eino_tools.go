package signup

// The tools that do not post or send, as Eino tools: what src/pi-agent.ts makes of them with functionTool.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/eino-contrib/jsonschema"
)

// einoSignupTool is one SignupTool as an Eino InvokableTool. callOf names the session and the call from the tool
// Step's context.
type einoSignupTool struct {
	tool   SignupTool
	info   *schema.ToolInfo
	callOf func(ctx context.Context) SignupToolCall
}

func (signupTool einoSignupTool) Info(context.Context) (*schema.ToolInfo, error) { return signupTool.info, nil }

func (signupTool einoSignupTool) InvokableRun(ctx context.Context, arguments string, _ ...tool.Option) (string, error) {
	return RunSignupTool(ctx, signupTool.tool, arguments, signupTool.callOf(ctx))
}

// EinoSignupTools are fetch_profile, github_user and finish_review as Eino tools, with the JSON Schemas the model
// sees.
func EinoSignupTools(services *Services, callOf func(ctx context.Context) SignupToolCall) ([]tool.BaseTool, error) {
	var tools []tool.BaseTool
	for _, signupTool := range SignupTools(services) {
		var parameters jsonschema.Schema
		if err := json.Unmarshal([]byte(signupTool.Schema), &parameters); err != nil {
			return nil, fmt.Errorf("tool %s schema: %w", signupTool.Name, err)
		}
		tools = append(tools, einoSignupTool{
			tool:   signupTool,
			info:   &schema.ToolInfo{Name: signupTool.Name, Desc: signupTool.Description, ParamsOneOf: schema.NewParamsOneOfByJSONSchema(&parameters)},
			callOf: callOf,
		})
	}
	return tools, nil
}
