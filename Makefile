REGISTRY ?= ghcr.io/amendezsap
IMAGE_NAME := gardener-extension-shoot-addon-service
ADMISSION_NAME := gardener-extension-admission-shoot-addon-service
TAG ?= $(shell cat VERSION)
CHART_DIR := charts/gardener-extension-shoot-addon-service
ADMISSION_CHART_DIR := charts/gardener-extension-admission-shoot-addon-service

.PHONY: build build-admission build-prepare ko-push ko-push-admission \
        helm-package helm-push helm-package-admission helm-push-admission \
        lint test tidy prepare pull-chart validate schema release

# ---------------------------------------------------------------------------
# Go
# ---------------------------------------------------------------------------

build:
	CGO_ENABLED=0 go build -o bin/$(IMAGE_NAME) ./cmd/$(IMAGE_NAME)

build-admission:
	CGO_ENABLED=0 go build -o bin/$(ADMISSION_NAME) ./cmd/$(ADMISSION_NAME)

build-prepare:
	CGO_ENABLED=0 go build -o bin/addon-prepare ./cmd/addon-prepare

test:
	go test ./... -v

lint:
	go vet ./...
	helm lint $(CHART_DIR)
	helm lint $(ADMISSION_CHART_DIR)

tidy:
	go mod tidy

# ---------------------------------------------------------------------------
# Container images (ko — no Dockerfiles needed)
# ---------------------------------------------------------------------------
#
# ko builds Go binaries into distroless containers and pushes to the registry.
# Default base image: gcr.io/distroless/static
# Override via .ko.yaml (local, not committed) or KO_DEFAULTBASEIMAGE env var.
#
# For air-gapped environments (e.g., with a private Harbor registry):
#   export KO_DEFAULTBASEIMAGE=harbor.example.com/distroless/static:nonroot
#   make ko-push REGISTRY=harbor.example.com/project

ko-push:
	KO_DOCKER_REPO=$(REGISTRY) ko build --bare --tags $(TAG) ./cmd/$(IMAGE_NAME)

ko-push-admission:
	KO_DOCKER_REPO=$(REGISTRY) ko build --bare --tags $(TAG) ./cmd/$(ADMISSION_NAME)

ko-push-list-containers:
	KO_DOCKER_REPO=$(REGISTRY) ko build --bare --tags $(TAG) ./cmd/list-containers

# ---------------------------------------------------------------------------
# Helm
# ---------------------------------------------------------------------------

helm-package:
	helm package $(CHART_DIR) -d .

helm-push: helm-package
	CHART_PKG=$$(ls $(IMAGE_NAME)-*.tgz | tail -1); \
	helm push "$$CHART_PKG" oci://$(REGISTRY)/charts; \
	rm -f "$$CHART_PKG"

helm-package-admission:
	helm package $(ADMISSION_CHART_DIR) -d .

helm-push-admission: helm-package-admission
	CHART_PKG=$$(ls $(ADMISSION_NAME)-*.tgz | tail -1); \
	helm push "$$CHART_PKG" oci://$(REGISTRY)/charts; \
	rm -f "$$CHART_PKG"

# ---------------------------------------------------------------------------
# Addon chart management (wraps addon-prepare Go tool)
# ---------------------------------------------------------------------------

prepare:
	go run ./cmd/addon-prepare prepare

pull-chart:
	go run ./cmd/addon-prepare pull-chart $(ARGS)

validate:
	go run ./cmd/addon-prepare validate

schema:
	go run ./cmd/addon-prepare schema

verify-prepare:
	go run ./cmd/addon-prepare prepare --verify

# ---------------------------------------------------------------------------
# Release
# ---------------------------------------------------------------------------

# Full release: prepare addons + push images + push charts
release: prepare ko-push ko-push-admission helm-push helm-push-admission
	@echo "Released $(REGISTRY)/$(IMAGE_NAME):$(TAG)"
	@echo "Released $(REGISTRY)/$(ADMISSION_NAME):$(TAG)"
