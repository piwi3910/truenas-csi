GO      ?= go
BIN     := bin/truenas-csi
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/pwatteel/truenas-csi/internal/driver.Version=$(VERSION)

.PHONY: build test lint clean

build:
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/truenas-csi

test:
	$(GO) test ./...

lint:
	$(GO) vet ./...

clean:
	rm -rf bin dist coverage.out
