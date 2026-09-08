GO      ?= go
BIN     := bin/truenas-csi
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X github.com/pwatteel/truenas-csi/internal/driver.Version=$(VERSION)

# E2E_GOTIMEOUT must outlast the ginkgo timeout inside run.sh, or `go test`
# kills the suite mid-run and the report is worthless.
E2E_GOTIMEOUT ?= 5h

.PHONY: build test lint clean e2e-external

build:
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/truenas-csi

test:
	$(GO) test ./...

# The upstream Kubernetes external-storage conformance suite. Opt-in: it needs a
# real cluster and a real appliance, and it skips itself out of `make test`
# unless TRUENAS_E2E_KUBECONFIG is set. See test/external/README.md.
e2e-external:
	$(GO) test ./test/external/ -run TestExternalStorageSuite -v -count=1 -timeout $(E2E_GOTIMEOUT)

lint:
	$(GO) vet ./...

clean:
	rm -rf bin dist coverage.out
