BINARY  := gpx-editor
# node_modules contains a stray Go package, so ./... is not usable here.
PKGS    := . ./internal/... ./web/...

.PHONY: all build frontend backend deps test check lint clean run cross

## build: frontend + single self-contained binary
all: build
build: frontend backend

## deps: install the npm toolchain (tsc, vite, eslint) if it is missing
deps: node_modules

# Everything frontend runs out of node_modules/.bin, so the targets that need
# those tools depend on the tree existing. Without this a fresh clone fails
# with a bare "tsc: command not found", which says nothing about the cause.
# The touch keeps the directory newer than the lockfile so this runs once.
node_modules: package-lock.json package.json
	npm ci
	@touch node_modules

frontend: node_modules
	npm run build

backend:
	go build -trimpath -ldflags "-s -w" -o $(BINARY) .

## run: build everything and serve on :8000
run: build
	./$(BINARY)

## test: Go tests plus the frontend logic harness
test: node_modules
	go test $(PKGS)
	npm run verify

## check: everything CI would run
check: test lint
lint: node_modules
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
