package main

import (
	"os"
	"path/filepath"
	"testing"
)

// .env fills in what the shell does not set, as Node's --env-file-if-exists does; nothing reaches os.Environ.
func TestDotEnvFillsInWhatTheShellDoesNotSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	contents := "# a comment\n\nREVIEW_API_URL=https://review.example\nSIGNUP_AGENT_HARNESS=pi\nexport SLACK_REVIEW_CHANNEL=\"C0123\"\nGITHUB_TOKEN=abc # a note\nBROKEN LINE\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIGNUP_AGENT_HARNESS", "eino")
	values, err := environment(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"REVIEW_API_URL": "https://review.example", "SIGNUP_AGENT_HARNESS": "eino", "SLACK_REVIEW_CHANNEL": "C0123", "GITHUB_TOKEN": "abc",
	} {
		if values[name] != want {
			t.Fatalf("%s = %q, want %q", name, values[name], want)
		}
	}
	if os.Getenv("REVIEW_API_URL") == "https://review.example" {
		t.Fatal(".env reached the process environment")
	}
	missing, err := environment(filepath.Join(t.TempDir(), "none"))
	if err != nil || missing["SIGNUP_AGENT_HARNESS"] != "eino" {
		t.Fatalf("without .env: %v", err)
	}
}
