BIN := gh-actions-pin
EXT_NAME := gh-actions-pin
# Honor XDG_DATA_HOME so this matches where gh actually resolves its data
# dir; fall back to the documented default when it's unset.
XDG_DATA_HOME ?= $(HOME)/.local/share
EXT_DIR := $(XDG_DATA_HOME)/gh/extensions/$(EXT_NAME)

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
