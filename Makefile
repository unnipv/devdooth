BIN      := bin/devdooth
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
LDFLAGS  := -X main.version=$(VERSION) -X github.com/unnipv/devdooth/internal/worker.Version=$(VERSION)

.PHONY: all build test race e2e smoke smoke-mcp vet fmt sync-landing release-snapshot clean

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/devdooth

test:
	go test ./... -count=1

race:
	go test -race ./internal/... -count=1

e2e:
	go test ./e2e/ -count=1 -v

vet:
	go vet ./...

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')

# Local demo: coordinator + worker + real Playwright over CDP.
smoke: build
	./scripts/smoke.sh

# Keep the GitHub Pages landing page and assets in sync with the embedded copies.
sync-landing:
	cp internal/webui/index.html docs/index.html
	cp internal/webui/assets/* docs/assets/

# Build every release archive locally to verify cross-compilation.
release-snapshot:
	./scripts/build-release.sh $(VERSION)

clean:
	rm -rf bin dist
