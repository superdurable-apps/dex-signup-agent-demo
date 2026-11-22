// Command signup-eino is the local process of the Eino harness: the Dex Worker of the session Flow
// SignupReviewEinoAgentFlow, the sign-up watcher and the Slack reply Trigger on Socket Mode, as npm start runs them
// for the TypeScript harnesses. It reads .env from the working directory into a copy of its environment, and stops on
// SIGTERM and SIGINT, the Worker after draining its tasks.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/superdurable/dex/blob-cache-go/blobcache"
	"github.com/superdurable/dex/sdk-go/dex"

	"github.com/superdurable-apps/dex-signup-agent-demo/eino/internal/signup"
)

const blobCacheBytes = 256 * 1024 * 1024

func main() {
	if err := run(); err != nil {
		slog.Error("application stopped", "error", err.Error())
		os.Exit(1)
	}
}

// environment is the process environment with .env applied: a variable already set wins, as Node's
// --env-file-if-exists does. Nothing is written back to the process environment.
func environment(path string) (map[string]string, error) {
	values := map[string]string{}
	file, err := os.Open(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			name, value, found := strings.Cut(strings.TrimPrefix(line, "export "), "=")
			if !found {
				continue
			}
			value = strings.TrimSpace(value)
			if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
				value = value[1 : len(value)-1]
			} else if index := strings.Index(value, " #"); index >= 0 {
				value = strings.TrimSpace(value[:index])
			}
			values[strings.TrimSpace(name)] = value
		}
		if err := scanner.Err(); err != nil {
			return nil, err
		}
	}
	for _, entry := range os.Environ() {
		if name, value, found := strings.Cut(entry, "="); found {
			values[name] = value
		}
	}
	return values, nil
}

func dexClientOptions(config signup.Config) (dex.ClientOptions, dex.WorkerOptions, error) {
	clientOptions := dex.ClientOptions{FlowServiceAddress: config.Dex.ServerAddress}
	workerOptions := dex.WorkerOptions{FlowServiceAddress: config.Dex.ServerAddress, Pull: &dex.PullWorkerOptions{MaxConcurrentTasks: config.WorkerSlots}}
	if config.Dex.TLS {
		clientOptions.FlowServiceTLS = &dex.FlowServiceTLSOptions{}
		workerOptions.FlowServiceTLS = &dex.FlowServiceTLSOptions{}
	}
	if config.Dex.APIKey != "" {
		var credentialOptions []dex.APIKeyCredentialOption
		if config.Dex.TokenCAFile != "" {
			client, err := httpClientTrusting(config.Dex.TokenCAFile)
			if err != nil {
				return dex.ClientOptions{}, dex.WorkerOptions{}, err
			}
			credentialOptions = append(credentialOptions, dex.WithAPIKeyHTTPClient(client))
		}
		credential, err := dex.APIKeyCredential(config.Dex.TokenURL, config.Dex.ServerID, config.Dex.APIKey, credentialOptions...)
		if err != nil {
			return dex.ClientOptions{}, dex.WorkerOptions{}, err
		}
		clientOptions.Credential, workerOptions.Credential = credential, credential
	}
	return clientOptions, workerOptions, nil
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	values, err := environment(".env")
	if err != nil {
		return fmt.Errorf("read .env: %w", err)
	}
	application, err := signup.LoadApplication(ctx, signup.ApplicationOptions{Environment: values})
	if err != nil {
		return err
	}
	clientOptions, workerOptions, err := dexClientOptions(application.Config)
	if err != nil {
		return err
	}
	cacheDirectory, err := filepath.Abs(application.Config.BlobCacheDirectory)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cacheDirectory, 0o755); err != nil {
		return err
	}
	cache, err := blobcache.New(&blobcache.Config{Dir: cacheDirectory, MaxBytes: blobCacheBytes})
	if err != nil {
		return err
	}
	defer cache.Close()
	registry, err := dex.NewRegistry([]dex.Flow{application.SessionFlow})
	if err != nil {
		return err
	}
	client, err := dex.NewClient(registry, cache, clientOptions)
	if err != nil {
		return err
	}
	defer client.Close()
	application.Connect(client)
	worker, err := dex.NewWorker(registry, cache, workerOptions)
	if err != nil {
		return err
	}
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Start() }()
	registered, cancelRegistered := context.WithTimeout(ctx, 30*time.Second)
	err = worker.WaitPullRegistered(registered)
	cancelRegistered()
	if err != nil {
		_ = worker.Stop(context.Background())
		return fmt.Errorf("register the pull Worker: %w", err)
	}
	running, err := application.Start(ctx, client)
	if err != nil {
		_ = worker.Stop(context.Background())
		return err
	}
	slog.Info(fmt.Sprintf("Signup review agent (%s) is running on %s", application.SessionFlow.GetFlowType(), application.Config.Dex.ServerAddress))
	var failure error
	select {
	case <-ctx.Done():
		slog.Info("Stopping; the reviews wait in Dex and go on when the agent starts again")
	case err := <-running.Done:
		failure = err
	case err := <-workerDone:
		failure = fmt.Errorf("the Dex Worker stopped: %v", err)
	}
	running.Stop()
	stopping, cancelStopping := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancelStopping()
	if err := worker.Stop(stopping); err != nil && failure == nil {
		failure = err
	}
	return failure
}

// httpClientTrusting is an HTTP client that trusts the system roots and the CA of caFile, for the token exchange
// (DEX_TOKEN_CA_FILE).
func httpClientTrusting(caFile string) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	contents, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("DEX_TOKEN_CA_FILE: %w", err)
	}
	if !pool.AppendCertsFromPEM(contents) {
		return nil, errors.New("DEX_TOKEN_CA_FILE holds no PEM certificate")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}, nil
}
