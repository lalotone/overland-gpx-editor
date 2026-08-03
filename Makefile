BINARY  := gpx-editor
# node_modules contains a stray Go package, so ./... is not usable here.
PKGS    := . ./internal/... ./web/...
# Stamped into the binary and reported by -version. Falls back to "dev" outside
# a git checkout, so a tarball build still says something honest.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build frontend backend deps test check lint clean run cross packages dist

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
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

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

# Every target is pure Go, so one Linux machine builds all of them with no
# cross-toolchain, no container and no macOS runner.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

## cross: binaries for every released platform, one directory each
cross: frontend
	@rm -rf build && mkdir -p build
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		out=build/$(BINARY)-$$os-$$arch; \
		bin=$(BINARY); [ "$$os" = windows ] && bin=$(BINARY).exe; \
		mkdir -p $$out; \
		echo "  $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$out/$$bin . || exit 1; \
		cp README.md LICENSE $$out/; \
	done

# Debian and RPM packages. nfpm writes both formats without dpkg or rpmbuild
# on the machine, so this runs on the same Linux box as everything else.
# Install it with: go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest
NFPM ?= nfpm
# Packages want the Debian architecture names, not Go's.
DEB_VERSION := $(patsubst v%,%,$(VERSION))

## packages: .deb and .rpm for amd64 and arm64
packages: cross
	@command -v $(NFPM) >/dev/null || { \
		echo "nfpm not found: go install github.com/goreleaser/nfpm/v2/cmd/nfpm@latest"; exit 1; }
	@mkdir -p build/pkgroot
	@for arch in amd64 arm64; do \
		cp build/$(BINARY)-linux-$$arch/$(BINARY) build/pkgroot/$(BINARY); \
		for format in deb rpm; do \
			ARCH=$$arch VERSION=$(DEB_VERSION) $(NFPM) package \
				--config packaging/nfpm.yaml --packager $$format --target build/ || exit 1; \
		done; \
	done
	@rm -rf build/pkgroot
	@ls -1 build/*.deb build/*.rpm | sed 's/^/  /'

## dist: the archives, packages and checksums that go on a GitHub release
dist: packages
	@cd build && for dir in $(BINARY)-*/; do \
		dir=$${dir%/}; \
		case $$dir in \
			*windows*) zip -qr $$dir.zip $$dir ;; \
			*)         tar czf $$dir.tar.gz $$dir ;; \
		esac; \
	done
	@cd build && sha256sum *.tar.gz *.zip *.deb *.rpm > SHA256SUMS
	@echo; echo "$(VERSION):"; ls -1sh build/*.tar.gz build/*.zip build/*.deb build/*.rpm build/SHA256SUMS | sed 's/^/  /'

clean:
	rm -rf $(BINARY) build web/dist/assets web/dist/index.html
