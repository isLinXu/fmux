SHELL := /bin/bash
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
LDFLAGS := -X github.com/isLinXu/fmux/internal/api.Version=$(VERSION)
GOBIN   := bin/fmux

.PHONY: all build test conformance e2e capture vet fmt run mock clean install launchagent service bench release-check

all: build

build:
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(GOBIN) ./cmd/fmux
	@echo "built $(GOBIN) ($(VERSION))"

test:
	go test ./...

conformance:
	go test ./test/conformance/ -v

# Process supervision against a real child process (needs python3).
e2e:
	go test ./test/e2e/ -v

# Re-record golden request bodies from the installed fm binary.
capture:
	python3 scripts/capture_fm.py

vet:
	go vet ./...

fmt:
	gofmt -l -w .

run: build
	./$(GOBIN) serve

mock: build
	./$(GOBIN) serve --mock --listen 127.0.0.1:1976

install: build
	install -d $(HOME)/.local/bin
	install -m 0755 $(GOBIN) $(HOME)/.local/bin/fmux
	@echo "installed $(HOME)/.local/bin/fmux"

launchagent: install
	./scripts/install-launchagent.sh

service: install
	$(HOME)/.local/bin/fmux service status

# Validate the release pipeline without publishing anything.
release-check:
	goreleaser check
	goreleaser build --snapshot --clean

bench:
	./scripts/bench.sh

clean:
	rm -rf bin
