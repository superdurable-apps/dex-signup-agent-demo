package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// .env fills in what the shell does not set, as Node's --env-file-if-exists does; nothing reaches os.Environ.
func TestDotEnvFillsInWhatTheShellDoesNotSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	contents := "# a comment\n\nREVIEW_API_URL=https://review.example\nSIGNUP_AGENT_HARNESS=pi\nexport SLACK_REVIEW_CHANNEL=\"C0123\"\nGITHUB_TOKEN=abc # a note\nBROKEN LINE\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	// The shell sets SIGNUP_AGENT_HARNESS and none of the names the .env fills in, whatever the shell that runs the
	// tests sets: a CI job often sets GITHUB_TOKEN.
	for _, name := range []string{"REVIEW_API_URL", "SLACK_REVIEW_CHANNEL", "GITHUB_TOKEN"} {
		unsetenv(t, name)
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

// unsetenv unsets name for the rest of the test; t.Setenv restores the shell's value afterwards.
func unsetenv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

// .env reads as Node's --env-file reads it, so a .env that works for npm start gives the Eino process the same values.
// The expected values are what Node 22's --env-file-if-exists gives for the same file.
func TestDotEnvReadsAsNodesEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	contents := "ZZ_A=\"C0123\" # review channel\nZZ_B=srk_abc#def\nZZ_C=\"line1\\nline2\"\nZZ_D=`tick`\nZZ_E='x y' # c\n" +
		"ZZ_F=\"multi\nline\"\n  # ZZ_G=no\nexport ZZ_H = spaced   # c\r\nZZ_I=\"unterminated\nZZ_J=after\nZZ_K=\nZZ_L='single \\n'\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := environment(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"ZZ_A": "C0123", "ZZ_B": "srk_abc", "ZZ_C": "line1\nline2", "ZZ_D": "tick", "ZZ_E": "x y", "ZZ_F": "multi\nline",
		"# ZZ_G": "no", "ZZ_H": "spaced", "ZZ_I": "\"unterminated", "ZZ_J": "after", "ZZ_K": "", "ZZ_L": "single \\n",
	} {
		if value, found := values[name]; !found || value != want {
			t.Errorf("%s = %q (found %v), want %q", name, value, found, want)
		}
	}
}

// A second Ctrl+C while the process stops, with the Worker draining its tasks for up to 40 seconds, ends it at once, as
// a second Ctrl+C ends npm start. The test runs itself as the process: SIGNUP_EINO_SIGNAL_TEST=child.
func TestASecondInterruptEndsTheProcessWhileItStops(t *testing.T) {
	if os.Getenv("SIGNUP_EINO_SIGNAL_TEST") == "child" {
		ctx, stop := stopSignalContext(context.Background())
		defer stop()
		fmt.Println("running")
		<-ctx.Done()
		fmt.Println("stopping")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	process := exec.Command(os.Args[0], "-test.run=^TestASecondInterruptEndsTheProcessWhileItStops$")
	process.Env = append(os.Environ(), "SIGNUP_EINO_SIGNAL_TEST=child")
	output, err := process.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Process.Kill() })
	lines := bufio.NewScanner(output)
	waitForLine := func(want string) {
		t.Helper()
		for lines.Scan() {
			if lines.Text() == want {
				return
			}
		}
		t.Fatalf("the process never printed %q", want)
	}
	waitForLine("running")
	_ = process.Process.Signal(os.Interrupt)
	waitForLine("stopping")
	_ = process.Process.Signal(os.Interrupt)
	ended := make(chan error, 1)
	go func() { ended <- process.Wait() }()
	select {
	case err := <-ended:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGINT {
			t.Fatalf("the process ended with %v, not by the second interrupt", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second interrupt did not end the process")
	}
}
