BINARY  := gpx-editor
# node_modules contains a stray Go package, so ./... is not usable here.
PKGS    := . ./internal/... ./web/...
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build frontend backend test check lint clean run cross

## build: frontend + single self-contained binary
all: build
build: frontend backend

frontend:
	npm run build

backend:
	go build -trimpath -ldflags "-s -w" -o $(BINARY) .

## run: build everything and serve on :8000
run: build
	./$(BINARY)

## test: Go tests plus the frontend logic harness
test:
	go test $(PKGS)
	npm run verify

## check: everything CI would run
check: test lint
lint:
	go vet $(PKGS)
	gofmt -l . | grep -v node_modules || true
	npx tsc --noEmit
	npm run lint

## cross: release binaries for the common targets
cross: frontend
	@mkdir -p build
	GOOS=linux  GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o build/$(BINARY)-linux-amd64 .
	GOOS=linux  GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o build/$(BINARY)-linux-arm64 .
	GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o build/$(BINARY)-darwin-arm64 .
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o build/$(BINARY)-windows-amd64.exe .

clean:
	rm -rf $(BINARY) build web/dist/assets web/dist/index.html
