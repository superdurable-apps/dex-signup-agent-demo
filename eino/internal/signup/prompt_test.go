package signup_test

import (
	"os"
	"strings"
	"testing"

	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signup"
)

// The Go prompt is an embedded copy of src/signup-agent.ts signupAgentPrompt. This test reads the template literal
// from the TypeScript source and fails when the copies differ, so they never drift: change the TypeScript prompt,
// then copy it into eino/internal/signup/prompt.txt.
func TestPromptIsTheTypeScriptPrompt(t *testing.T) {
	source, err := os.ReadFile("../../../src/signup-agent.ts")
	if err != nil {
		t.Fatalf("read the TypeScript prompt: %v", err)
	}
	const opening = "export const signupAgentPrompt = `"
	text := string(source)
	start := strings.Index(text, opening)
	if start < 0 {
		t.Fatalf("src/signup-agent.ts has no %q", opening)
	}
	start += len(opening)
	end := strings.Index(text[start:], "`;")
	if end < 0 {
		t.Fatal("the signupAgentPrompt template literal has no end")
	}
	literal := text[start : start+end]
	// The copy holds the literal's text as JavaScript reads it; the prompt uses no escapes or substitutions.
	if strings.Contains(literal, "\\") || strings.Contains(literal, "${") {
		t.Fatal("the TypeScript prompt now uses escapes or substitutions: extend this test to read them")
	}
	if signup.SignupAgentPrompt != literal {
		t.Fatalf("eino/internal/signup/prompt.txt differs from src/signup-agent.ts signupAgentPrompt: copy the TypeScript prompt into prompt.txt")
	}
}
