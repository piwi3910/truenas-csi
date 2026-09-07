GO           ?= go
IMG          ?= ghcr.io/piwi3910/truenas-csi-operator:0.1.0
BUNDLE_IMG   ?= ghcr.io/piwi3910/truenas-csi-operator-bundle:0.1.0
ENVTEST_K8S  ?= 1.31.0

.PHONY: all build test manifests generate fmt vet envtest bundle-validate docker-build clean

all: generate manifests fmt vet build test

build:
	$(GO) build -o bin/manager ./cmd

# generate rewrites zz_generated.deepcopy.go. Run it after any change to the API
# types, or the operator will not compile.
generate:
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen object:headerFile=/dev/null paths=./api/...

# manifests regenerates the CRD and the ClusterRole from the kubebuilder markers.
# The CRD in config/crd/bases is what the API server enforces, and what the
# validation tests read: a marker that does not survive this step is a rule that
# does not exist in a cluster.
manifests:
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen \
		crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	$(GO) run sigs.k8s.io/controller-tools/cmd/controller-gen \
		rbac:roleName=truenas-csi-operator paths=./internal/controller/... output:rbac:artifacts:config=config/rbac

fmt:
	gofmt -l -w .

vet:
	$(GO) vet ./...

# test runs without a cluster. The envtest-backed cases skip clearly when the
# API server binaries are not present; set KUBEBUILDER_ASSETS to run them.
test:
	$(GO) test ./...

# envtest downloads the kube-apiserver and etcd binaries and prints the export
# line that turns the skipped API-server tests on.
envtest:
	@$(GO) run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use $(ENVTEST_K8S) -p path

docker-build:
	docker build -f Dockerfile -t $(IMG) ..

# bundle-validate checks the OLM bundle with operator-sdk when it is installed.
bundle-validate:
	@command -v operator-sdk >/dev/null 2>&1 \
		&& operator-sdk bundle validate ./bundle \
		|| echo "operator-sdk not installed; skipping bundle validation"

clean:
	rm -rf bin
