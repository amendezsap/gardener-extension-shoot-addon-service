# gardener-extension-shoot-addon-service

[![REUSE status](https://api.reuse.software/badge/github.com/amendezsap/gardener-extension-shoot-addon-service)](https://api.reuse.software/info/github.com/amendezsap/gardener-extension-shoot-addon-service)

A [Gardener](https://gardener.cloud) extension that deploys user-defined Helm chart addons to shoot clusters. Bring your own charts — the extension handles the lifecycle.

## Documentation

- [Usage Guide](docs/usage.md) — Addon manifests, chart sources, deployment targets, values layering
- [Deployment Guide](docs/deployment.md) — Building, pushing, deploying to Gardener
- [Development Guide](docs/development.md) — Local setup, testing, adding addons
- [Registry Credentials](docs/registry-credentials.md) — Image pull authentication patterns
- [Build-Time Auth](docs/build-time-auth.md) — Authenticated chart pulling during builds

## How It Works

1. **You add charts** — clone this repo, pull Helm charts into `charts/embedded/addons/`, and declare them in `manifest.yaml`
2. **You build** — `go:embed` compiles your charts into the binary. No runtime registry dependencies.
3. **You deploy** — the extension runs on each seed and deploys your addons to every shoot as Gardener ManagedResources

```
┌──────────────┐     ┌──────────────┐     ┌───────────────┐
│ Your charts  │────▶│  go:embed    │────▶│  Extension     │
│ + values     │     │  at build    │     │  renders &     │
│ + manifest   │     │  time        │     │  deploys to    │
│              │     │              │     │  every shoot   │
└──────────────┘     └──────────────┘     └───────────────┘
```

## Architecture

The extension consists of two separately deployed pods on each seed:

```
Seed Cluster
├── extension-shoot-addon-service namespace
│   ├── gardener-extension-shoot-addon-service          (controller pod)
│   │   ├── Leader election participant
│   │   ├── Reconciles Extension resources for each shoot
│   │   ├── Renders and deploys addon ManagedResources
│   │   ├── Manages AWS infrastructure (IAM, VPC endpoints)
│   │   └── Auto-fixes stale GRM ConfigMaps on reconcile
│   │
│   └── gardener-extension-admission-shoot-addon-service (admission pod)
│       ├── No leader election — always running and ready
│       ├── MutatingWebhookConfiguration for GRM ConfigMaps
│       ├── Removes targetClientConnection.namespaces restriction at CREATE
│       └── Enables ManagedResources to deploy to custom namespaces
```

**Controller pod** (`cmd/gardener-extension-shoot-addon-service/`): The main reconciliation loop. Watches Extension resources, renders Helm charts from embedded addons, creates ManagedResources, and manages AWS infrastructure. Participates in leader election like all Gardener extension controllers.

**Admission pod** (`cmd/gardener-extension-admission-shoot-addon-service/`): A dedicated webhook server that intercepts GRM ConfigMap creation and removes the namespace restriction. Runs independently of the controller pod with no leader election dependency, so it is always ready to intercept ConfigMap creates even during controller rollouts or restarts.

**Why separate pods?** Gardenlet creates the GRM ConfigMap at step 6 (DeployControlPlane) of the shoot reconciliation DAG. If the webhook lives inside the controller pod, it only becomes ready after leader election completes. During a rollout or pod restart, there is a timing window where the ConfigMap is created before the webhook is registered, causing the namespace restriction to slip through. The admission pod eliminates this race. This is the standard pattern used by provider-aws, ACL, dns-service, cert-service, and all other Gardener extensions that have webhooks.

## Features

- **Any Helm chart** — Fluent Bit, Prometheus, nginx, your internal tools — anything that's a Helm chart
- **Multiple chart sources** — OCI registries, Helm repos, Git repos, local paths
- **Air-gap friendly** — everything embeds at build time, zero runtime network dependencies
- **AWS infrastructure** — optional IAM policy attachment and VPC endpoint management per addon
- **Per-shoot overrides** — shoots can enable/disable addons or toggle features via `providerConfig`
- **GRM namespace provisioner** — in-process webhook ensures ManagedResources can deploy to any namespace

## Quick Start

```bash
# 1. Clone
git clone https://github.com/amendezsap/gardener-extension-shoot-addon-service
cd gardener-extension-shoot-addon-service

# 2. Pull a chart
make pull-chart NAME=fluent-bit \
  OCI=oci://ghcr.io/fluent/helm-charts/fluent-bit \
  VERSION=0.56.0

# 3. Add your values
cp examples/addons/fluent-bit/values/* charts/embedded/addons/fluent-bit/values/

# 4. Declare it in the manifest
cp examples/manifests/minimal.yaml charts/embedded/addons/manifest.yaml

# 5. Build and push
make release REGISTRY=ghcr.io/your-org

# 6. Deploy to Gardener
ytt -f examples/ytt/ \
  -v registry=ghcr.io/your-org \
  -v version=$(cat VERSION) \
  | kubectl apply -f -
```

## Chart Sources

The Makefile supports pulling charts from any source:

```bash
# OCI registry (ghcr.io, Harbor, ECR, etc.)
make pull-chart NAME=fluent-bit \
  OCI=oci://ghcr.io/fluent/helm-charts/fluent-bit \
  VERSION=0.56.0

# Helm repository
make pull-chart NAME=fluent-bit \
  REPO=https://fluent.github.io/helm-charts \
  CHART=fluent-bit \
  VERSION=0.48.6

# Git repository (GitHub, GitLab, any git host)
make pull-chart NAME=fluent-bit \
  GIT=https://github.com/fluent/helm-charts \
  GIT_PATH=charts/fluent-bit \
  GIT_REF=main

# Local path
make pull-chart NAME=my-app PATH=/path/to/my/chart
```

## Addon Manifest

The manifest (`charts/embedded/addons/manifest.yaml`) declares what the extension deploys:

```yaml
apiVersion: addons.gardener.cloud/v1alpha1
kind: AddonManifest

# Default namespace for all addons on shoots
defaultNamespace: observability

addons:
  - name: fluent-bit
    chart:
      path: fluent-bit/chart         # relative to addons/ dir
    valuesPath: fluent-bit/values     # values overlay directory
    enabled: true

    # Values injected from shoot metadata at render time
    shootValues:
      fullnameOverride: addon-fluent-bit

    # Image override — configurable via Helm values without rebuild
    image:
      valuesKey: image

    # AWS infrastructure provisioned alongside this addon (optional)
    aws:
      iamPolicies:
        - CloudWatchAgentServerPolicy
      vpcEndpoint:
        service: logs
```

See `examples/manifests/` for more patterns:
- `minimal.yaml` — single addon, local chart
- `observability.yaml` — Fluent Bit + container report with AWS infra
- `full.yaml` — multiple addons from different sources (OCI, git, Helm repo)

## Directory Structure

```
charts/embedded/addons/           # YOUR CONTENT — add charts here
├── manifest.yaml                 # declares what to deploy
├── fluent-bit/                   # one directory per addon
│   ├── chart/                    # the Helm chart (pulled via make pull-chart)
│   │   ├── Chart.yaml
│   │   ├── templates/
│   │   └── values.yaml
│   └── values/                   # your values overlays
│       ├── values.yaml           # base values
│       └── values.aws.yaml       # provider-specific (optional)
└── another-addon/
    ├── chart/
    └── values/
```

## Per-Shoot Configuration

Shoots can override addon behavior via `providerConfig`:

```yaml
spec:
  extensions:
  - type: shoot-addon-service
    providerConfig:
      addons:
        fluent-bit:
          enabled: false          # disable on this shoot
        container-report:
          enabled: true           # enable on this shoot
      aws:
        vpcEndpoint:
          enabled: true           # use VPC endpoint instead of NAT
```

Or disable the extension entirely:

```yaml
spec:
  extensions:
  - type: shoot-addon-service
    disabled: true
```

## AWS Features (provider-aws only)

AWS infrastructure management **only applies to shoots using `provider-aws`**. Non-AWS shoots (GCP, OpenStack, etc.) get chart deployment only — IAM and VPC endpoint steps are skipped automatically.

> **Future:** GCP IAM equivalent (Workload Identity, service account binding) is planned but not yet implemented.

**Global IAM Policies** — node-level, attached regardless of which addons are enabled:
```yaml
globalAWS:
  iamPolicies:
    - CloudWatchAgentServerPolicy
    - AmazonSSMManagedInstanceCore
```

**Per-Addon VPC Endpoints** — created in the shoot's VPC with the Gardener node security group:
```yaml
addons:
  - name: fluent-bit
    aws:
      vpcEndpoint:
        service: logs    # → com.amazonaws.<region>.logs
```

VPC endpoints support shared VPCs (tag-based tracking prevents premature deletion) and are configurable per-shoot via `providerConfig`.

IAM policies removed from `globalAWS.iamPolicies` are automatically detached on the next reconcile (stale policy detection via ProviderStatus).

## Values Layering

For each addon, values are merged in order (last wins):

1. Chart's built-in `values.yaml`
2. Your `values/values.yaml` overlay
3. Your `values/values.<provider>.yaml` (e.g., `values.aws.yaml`)
4. `shootValues` from the manifest
5. Image overrides from Helm values (env vars)

## Documentation

- [Registry Credentials & Image Pull Configuration](docs/registry-credentials.md) --
  Three patterns for runtime image pull auth (node-level containerd, extension-deployed
  Secrets, Kyverno webhook), manifest configuration reference, credential flow
  diagram, security model, FAQ.
- [Build-Time Authentication for Chart Pulling](docs/build-time-auth.md) --
  OCI registry login, Helm repo env vars, Git token/SSH auth, CI/CD examples
  for GitHub Actions / GitLab CI / CodeBuild.

## Prerequisites

- Go 1.25+
- [ko](https://ko.build/) — container image builder for Go (`go install github.com/google/ko@latest`)
- [ytt](https://carvel.dev/ytt/) — YAML templating for operator Extension manifests
- [Helm](https://helm.sh/) 3.x — chart packaging and pushing
- Gardener v1.80+ with operator Extensions support

## Development

```bash
make build              # local Go build (controller)
make build-admission    # local Go build (admission webhook)
make test               # run tests
make lint               # go vet + helm lint
make pull-chart ...     # pull a chart into addons/

# Container images (ko — no Dockerfiles)
make ko-push            # build + push controller image
make ko-push-admission  # build + push admission image

# Full release (images + Helm charts)
make release REGISTRY=ghcr.io/your-org
```

For air-gapped environments, override the base image:
```bash
export KO_DEFAULTBASEIMAGE=your-registry.example.com/distroless/static:nonroot
make release REGISTRY=your-registry.example.com/project
```

## License

Apache License 2.0 — see [LICENSES/Apache-2.0.txt](LICENSES/Apache-2.0.txt).
