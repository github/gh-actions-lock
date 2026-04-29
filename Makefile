BIN := gh-actions-pin

.PHONY: build test install reinstall

build:
	go build -o $(BIN) ./cmd/gh-actions-pin

test:
	go test ./...

install: build
	gh extension install .

reinstall: build
	-gh extension remove actions-pin
	gh extension install .
