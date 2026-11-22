SHELL := /usr/bin/env bash
# RUN selects the API tests of make test-app by name, as a regular expression.
RUN ?=
# One test file at a time, so the tests' report never races; each test gets
# its own engine and at most five minutes.
TEST_FLAGS := --test --test-concurrency=1 --test-timeout=300000 $(if $(RUN),--test-name-pattern='$(RUN)')
TESTS := 'apptest/*.test.ts'
# The app tests' engine; superverse-previewctl names it, and outside a run the
# dex-server-zd on PATH serves.
DEX_SERVER_ZD_BINARY ?= $(shell command -v dex-server-zd)
export DEX_SERVER_ZD_BINARY

.PHONY: bootstrap restored session-graphs check-fdg-v2 check-static test-app test eino-dex run-eino test-eino
# The platform starts bootstrap with the workspace; a second run waits for the
# first, and every other target waits for a run in progress.
bootstrap:
	./scripts/bootstrap.sh
restored:
	@mkdir -p .local && if command -v flock >/dev/null; then flock -s .local/bootstrap.lock true; fi
# Writes both harnesses' session graphs with the Slack reply Trigger binding; check-fdg-v2 checks them.
session-graphs: restored
	npx --no-install dex-agents-session-graph --flow-type SignupReviewPiAgentFlow --permission signup-review.operate \
		--trigger-bindings scripts/session-trigger-bindings.json --out flow-definitions/signup-review-pi-agent.json
	npx --no-install dex-agents-session-graph --flow-type SignupReviewClaudeAgentFlow --permission signup-review.operate \
		--trigger-bindings scripts/session-trigger-bindings.json --out flow-definitions/signup-review-claude-agent.json
check-fdg-v2: restored
	python3 ./scripts/render-flow-definitions.py
check-static: check-fdg-v2
	npx --no-install tsc -p tsconfig.json
	npm run build
# The API tests run against a real engine: outside a run, superverse-previewctl
# starts the run that provides it.
test-app: restored
	@if [ -z "$${SUPERVERSE_APPTEST_RUN:-}" ] && command -v superverse-previewctl >/dev/null; then \
		exec superverse-previewctl apptest run -- node $(TEST_FLAGS) $(TESTS); \
	fi; \
	node $(TEST_FLAGS) $(TESTS)
# The scenario suite of both harnesses (docs/reference.md, Tests); it needs dexcli and dex-server-zd on PATH.
test: restored
	npm test
# The Eino harness (eino/, Go, local only): run-eino builds and runs its local process with .env; test-eino runs its
# unit tests, scenario suite and crash tests on private Dex engines of DEXCLI with the dex-server-zd beside it
# (graceful completion needs dexcli 1.47.9 or later, with server-zd 0.1.34), not test-app's DEX_SERVER_ZD_BINARY;
# EINO_DEX_SERVER_ZD_BINARY names another one. RUN selects tests by name, as a go test -run regular expression. A
# relative path in DEXCLI or EINO_DEX_SERVER_ZD_BINARY is relative to where make runs, since the tests run in eino/.
DEXCLI ?= $(shell command -v dexcli)
export DEXCLI
EINO_DEX_SERVER_ZD_BINARY ?=
# Until Dex releases its Go modules agents/go/core and agents/go/eino, eino/go.mod replaces both with .local/dex.
# EINO_DEX_CHECKOUT names a Dex checkout that has both (a relative path is relative to where make runs); run-eino and
# test-eino link it at .local/dex, and later runs, and go commands in eino/, use that link. Each run prints the
# checkout's commit, since go.sum pins no replaced module.
EINO_DEX_CHECKOUT ?=
eino-dex:
	@if [ -n '$(EINO_DEX_CHECKOUT)' ]; then \
		for module in core eino; do \
			if [ ! -f '$(abspath $(EINO_DEX_CHECKOUT))'/agents/go/$$module/go.mod ]; then \
				echo "EINO_DEX_CHECKOUT=$(EINO_DEX_CHECKOUT) has no agents/go/$$module/go.mod: name a Dex checkout that has the Go modules agents/go/core and agents/go/eino" >&2; \
				exit 1; \
			fi; \
		done; \
		if [ -e .local/dex ] && [ ! -L .local/dex ]; then \
			echo ".local/dex is not a link: remove it, then run make again" >&2; exit 1; \
		fi; \
		mkdir -p .local && ln -sfn '$(abspath $(EINO_DEX_CHECKOUT))' .local/dex; \
	elif [ ! -f .local/dex/agents/go/core/go.mod ] || [ ! -f .local/dex/agents/go/eino/go.mod ]; then \
		echo "The Eino harness builds against the Dex Go modules agents/go/core and agents/go/eino, which Dex has not released yet." >&2; \
		echo "Name a Dex checkout that has both once, such as: make $(or $(MAKECMDGOALS),test-eino) EINO_DEX_CHECKOUT=../dex" >&2; \
		exit 1; \
	fi; \
	echo "Dex Go modules: $$(cd .local/dex && pwd -P) at $$(git -C .local/dex describe --always --dirty --abbrev=12 --exclude='*' 2>/dev/null || echo 'a commit git cannot read')"
run-eino: eino-dex
	cd eino && GOWORK=off go build -o ../.local/bin/signup-eino ./cmd/signup-eino
	SIGNUP_AGENT_HARNESS=eino ./.local/bin/signup-eino
test-eino: eino-dex
	cd eino && GOWORK=off go vet ./... && DEXCLI='$(abspath $(DEXCLI))' DEX_SERVER_ZD_BINARY='$(abspath $(EINO_DEX_SERVER_ZD_BINARY))' \
		GOWORK=off go test -count=1 -timeout 30m $(if $(RUN),-run '$(RUN)') ./...
