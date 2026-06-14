BIN := gh-actions-lock
EXT_NAME := gh-actions-lock
# Honor XDG_DATA_HOME so this matches where gh actually resolves its data
# dir; fall back to the documented default when it's unset.
XDG_DATA_HOME ?= $(HOME)/.local/share
EXT_DIR := $(XDG_DATA_HOME)/gh/extensions/$(EXT_NAME)

RUBY := $(shell command -v /opt/homebrew/opt/ruby/bin/ruby 2>/dev/null || echo ruby)

.PHONY: build test test-integration test-shell test-live test-matrix test-smoke test-stub test-real install reinstall uninstall

build:
	go build -o $(BIN) ./cmd/gh-actions-lock

test:
	go test ./...

test-integration: build
	$(RUBY) test/integration/run.rb

test-shell: build
	$(RUBY) test/integration/run.rb --shell

test-live: build
	$(RUBY) test/integration/run.rb --live

test-smoke: build
	$(RUBY) test/integration/run.rb --smoke

test-stub: build
	$(RUBY) test/integration/run.rb --stub

test-real: build
	$(RUBY) test/integration/run.rb --real

test-matrix: build
	$(RUBY) test/integration/run.rb --matrix

# install/reinstall work from any checkout — main repo or worktree — by
# placing the built binary directly into gh's extension dir. We skip
# `gh extension install .` because it rejects worktree directories whose
# basename doesn't start with `gh-`.
install: build
	@mkdir -p $(EXT_DIR)
	cp $(BIN) $(EXT_DIR)/$(EXT_NAME)

reinstall: uninstall install

uninstall:
	rm -rf $(EXT_DIR)
