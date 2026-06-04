BIN := gh-actions-pin
EXT_NAME := gh-actions-pin
EXT_DIR := $(HOME)/.local/share/gh/extensions/$(EXT_NAME)

.PHONY: build test install reinstall uninstall

build:
	go build -o $(BIN) ./cmd/gh-actions-pin

test:
	go test ./...

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
