SHELL := /bin/bash

# Image coordinates. Override REGISTRY or IMAGE to publish somewhere else.
REGISTRY    ?= ghcr.io/zzhkhyl
IMAGE       ?= $(REGISTRY)/statfs-guard-operator
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
NAMESPACE   ?= statfs-guard

# Pinned so a broken default proxy or an unexpected toolchain download cannot
# hang the build. Override any of the three from the environment (CI does).
GOPROXY     ?= https://goproxy.cn,direct
GOSUMDB     ?= off
GOTOOLCHAIN ?= local
GO          := env GOPROXY=$(GOPROXY) GOSUMDB=$(GOSUMDB) GOTOOLCHAIN=$(GOTOOLCHAIN) go

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'

.PHONY: tidy
tidy: ## Resolve dependencies
	$(GO) mod tidy

.PHONY: fmt
fmt: ## gofmt the tree
	$(GO) fmt ./...

.PHONY: vet
vet: ## Static checks for linux/amd64
	GOOS=linux GOARCH=amd64 $(GO) vet ./...

.PHONY: test
test: ## Unit tests
	$(GO) test ./... -count=1

.PHONY: build
build: ## Build linux/amd64 binaries into dist/
	mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/manager ./cmd/manager
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o dist/agent   ./cmd/agent
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w"                                -o dist/prober  ./cmd/prober
	@ls -lh dist/

.PHONY: image
image: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

.PHONY: push
push: ## Push the container image
	docker push $(IMAGE):$(VERSION)
	docker push $(IMAGE):latest

.PHONY: bundle
bundle: ## Regenerate deploy/{install,rules,monitoring}.yaml from config/
	python3 hack/bundle.py

.PHONY: bundle-check
bundle-check: bundle ## Fail when the generated bundles are behind config/
	@git diff --exit-code -- deploy/ >/dev/null || { echo "deploy/ is stale: run 'make bundle' and commit the result"; exit 1; }

.PHONY: verify
verify: ## Everything CI runs: format, vet, both GOOS builds, tests, bundle freshness
	@test -z "$$(gofmt -l .)" || { echo "gofmt reported files:"; gofmt -l .; exit 1; }
	GOOS=linux   GOARCH=amd64 $(GO) vet ./...
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 $(GO) build ./...
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build ./...
	$(GO) test ./... -count=1
	$(MAKE) bundle-check

.PHONY: install
install: bundle ## Apply CRD, RBAC, agent and manager
	kubectl apply -f deploy/install.yaml

.PHONY: install-monitoring
install-monitoring: bundle ## Apply the VictoriaMetrics scrape configs and alert rules
	kubectl apply -f deploy/monitoring.yaml

.PHONY: uninstall
uninstall: ## Remove everything, including the CRD
	kubectl delete -f deploy/monitoring.yaml --ignore-not-found
	kubectl delete -f deploy/install.yaml --ignore-not-found
	kubectl delete crd cephfsstatfsreports.storage.statfsguard.io --ignore-not-found

.PHONY: reports
reports: ## Show the current drift table
	kubectl get cephfsstatfsreports -o wide
