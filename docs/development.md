# Development Guide

How to set up a local development environment, build, test, and contribute.

## Table of Contents

- [Prerequisites](#prerequisites)
- [Repository Structure](#repository-structure)
- [Building](#building)
- [Testing](#testing)
- [Adding an Addon](#adding-an-addon)
- [The Addon Prepare Tool](#the-addon-prepare-tool)
- [Debugging](#debugging)

## Prerequisites

- Go 1.25+
- [ko](https://ko.build/) — `go install github.com/google/ko@latest`
- Helm 3.x
- Access to a Gardener landscape (for integration testing)

## Repository Structure

```
.
├── cmd/
│   ├── gardener-extension-shoot-addon-service/     # Controller binary
│   ├── gardener-extension-admission-shoot-addon-service/  # Admission webhook binary
│   └── addon-prepare/                              # Build-time tool for chart management
├── pkg/
│   ├── addon/        # Addon manifest types and parsing
│   ├── apis/config/  # Extension providerConfig types
│   ├── aws/          # AWS client (IAM, VPC endpoints)
│   ├── controller/addon/  # Main reconciliation logic (actuator.go)
│   └── webhook/grm/      # GRM namespace injection webhook
├── charts/
│   ├── gardener-extension-shoot-addon-service/     # Extension Helm chart
│   ├── gardener-extension-admission-shoot-addon-service/  # Admission charts (virtual garden RBAC)
│   └── embedded/
│       └── addons/        # Embedded addon charts + manifest (go:embed)
├── examples/              # Example addon definitions and manifests
│   └── addons/
│       ├── fluent-bit/
│       ├── container-report/
│       └── nginx/
└── docs/                  # Documentation
```

## Building

```bash
# Build controller binary
make build

# Build admission webhook binary
make build-admission

# Build addon-prepare tool
make build-prepare

# Run linter (go vet + helm lint)
make lint

# Run tests
make test

# Tidy Go modules
make tidy
```

### Container Images

```bash
# Push controller image
make ko-push REGISTRY=ghcr.io/your-org TAG=latest

# Push admission image
make ko-push-admission REGISTRY=ghcr.io/your-org TAG=latest

# Push Helm chart
make helm-push REGISTRY=ghcr.io/your-org

# Full release (prepare + push all)
make release REGISTRY=ghcr.io/your-org TAG=v0.1.0
```

## Testing

```bash
# Unit tests
make test

# Lint
make lint

# Validate addon manifest
make validate

# Verify embedded addons are prepared
make verify-prepare
```

## Adding an Addon

### Step 1: Create the addon directory

```bash
mkdir -p addons/my-addon/chart addons/my-addon/values
```

### Step 2: Add the Helm chart

Either copy a chart directly or use the prepare tool:

```bash
# Pull from OCI registry
make pull-chart ARGS="--oci oci://registry.example.com/charts/my-chart --version 1.0.0 --output addons/my-addon/chart"

# Or copy a local chart
cp -r /path/to/my-chart/* addons/my-addon/chart/
```

### Step 3: Add values

Create `addons/my-addon/values/values.yaml` with base values, and optionally `values.aws.yaml` for provider-specific overrides.

### Step 4: Declare in manifest

Add the addon to `addons/manifest.yaml`:

```yaml
addons:
  - name: my-addon
    chart:
      path: my-addon/chart
    valuesPath: my-addon/values
    enabled: true
    target: shoot          # or seed, or global
    managedResourceName: my-addon
    shootValues:
      fullnameOverride: my-addon
```

### Step 5: Prepare and build

```bash
make prepare    # Copies addons to charts/embedded/addons/
make validate   # Validates the manifest
make build      # Builds the binary with embedded charts
```

## The Addon Prepare Tool

The `addon-prepare` tool (`cmd/addon-prepare/`) manages addon chart lifecycle:

```bash
# Prepare all addons (copy to embedded directory)
make prepare

# Pull a chart from a remote source
make pull-chart ARGS="--oci oci://registry.example.com/charts/mychart --version 1.0 --output addons/mychart/chart"

# Validate the addon manifest
make validate

# Generate JSON schema for the manifest
make schema

# Verify embedded addons match source (CI check)
make verify-prepare
```

## Debugging

### Extension Logs

```bash
# On the seed cluster, find the extension namespace
kubectl get pods -A | grep shoot-addon

# Controller logs
kubectl logs -n <extension-namespace> -l app.kubernetes.io/name=gardener-extension-shoot-addon-service

# Admission webhook logs
kubectl logs -n <extension-namespace> -l app.kubernetes.io/name=shoot-addon-admission
```

### ManagedResource Status

```bash
# On the seed, check MR status for a shoot
kubectl get managedresource -n shoot--<project>--<shoot-name>

# Check MR conditions
kubectl get managedresource -n shoot--<project>--<shoot-name> <mr-name> -o jsonpath='{.status.conditions}'
```

### GRM ConfigMap

```bash
# Check if the GRM ConfigMap has the required namespaces
kubectl get cm -n shoot--<project>--<shoot-name> -l resources.gardener.cloud/garbage-collectable-reference=true | grep gardener-resource-manager

# Inspect the config
kubectl get cm -n shoot--<project>--<shoot-name> <cm-name> -o jsonpath='{.data.config\.yaml}' | grep -A5 targetClientConnection
```

### Common Issues

| Symptom | Cause | Fix |
|---|---|---|
| ManagedResource stuck `Unknown` | GRM not watching addon namespace | Check GRM ConfigMap for `targetClientConnection.namespaces` |
| Admission webhook not firing | Service name mismatch | Verify `admission.webhookName` matches Go `Name` constant |
| Pods `ImagePullBackOff` | Version bumped without pushing all images | Run `make release` to push both controller + admission |
| Extension `Error` status | Stale GRM ConfigMap detected | Expected — extension aborts so gardenlet retries from scratch |
