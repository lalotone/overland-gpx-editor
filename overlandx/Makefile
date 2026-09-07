BINARY  := overlandx
VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build frontend test check clean

all: build

build: frontend
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

frontend:
	VITE_API_BASE= VITE_ELEVATION_API= $(MAKE) -C .. frontend

test:
	go test ./...

check: test
	go vet ./...
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }

clean:
	rm -f $(BINARY)
