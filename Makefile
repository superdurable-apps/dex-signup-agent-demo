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

.PHONY: bootstrap restored check-fdg-v2 check-static test-app test
# The platform starts bootstrap with the workspace; a second run waits for the
# first, and every other target waits for a run in progress.
bootstrap:
	./scripts/bootstrap.sh
restored:
	@mkdir -p .local && if command -v flock >/dev/null; then flock -s .local/bootstrap.lock true; fi
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
# The scenario suite of both harnesses (README, Tests); it needs dexcli and dex-server-zd on PATH.
test: restored
	npm test
