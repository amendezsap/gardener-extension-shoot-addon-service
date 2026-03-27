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

The binary ships bare -- `charts/embedded/addons/` is empty by default. Addon configuration is provided at deploy time through the operator Extension CR.

1. **You configure addons** in the Extension CR's `values.addons` section -- OCI chart references, versions, and values
2. **You deploy** the Extension CR to the runtime cluster. The operator creates a ConfigMap on each seed with your addon config.
3. **The extension pulls charts** from OCI registries at runtime and deploys them to every shoot as Gardener ManagedResources

```
┌──────────────┐     ┌──────────────┐     ┌───────────────┐
│ Extension CR │────▶│  ConfigMap   │────▶│  Extension     │
│ values.addons│     │  on each     │     │  pulls charts  │
│ (OCI refs +  │     │  seed        │     │  from OCI &    │
│  values)     │     │              │     │  deploys to    │
│              │     │              │     │  every shoot   │
└──────────────┘     └──────────────┘     └───────────────┘
```

### Two Deployment Methods

**Runtime config (recommended):** Addon definitions live in the Extension CR. Charts are pulled from OCI registries at runtime. The same binary works everywhere -- no custom builds per environment.

**Embedded (self-contained fallback):** Pull charts into `charts/embedded/addons/`, declare them in `manifest.yaml`, and build with `go:embed`. Useful when you need a fully self-contained binary with zero runtime registry dependencies.

The extension uses a fallback chain: ConfigMap (runtime) -> embedded charts -> nothing.

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

**Controller pod** (`cmd/gardener-extension-shoot-addon-service/`): The main reconciliation loop. Watches Extension resources, loads addon config from the seed ConfigMap (falling back to embedded charts), pulls charts from OCI registries, renders them, creates ManagedResources, and manages AWS infrastructure. Participates in leader election like all Gardener extension controllers. On managed seeds, the extension skips seed-targeted addon deployment (the parent seed handles it).

**Admission pod** (`cmd/gardener-extension-admission-shoot-addon-service/`): A dedicated webhook server that intercepts GRM ConfigMap creation and removes the namespace restriction. Runs independently of the controller pod with no leader election dependency, so it is always ready to intercept ConfigMap creates even during controller rollouts or restarts.

**Why separate pods?** Gardenlet creates the GRM ConfigMap at step 6 (DeployControlPlane) of the shoot reconciliation DAG. If the webhook lives inside the controller pod, it only becomes ready after leader election completes. During a rollout or pod restart, there is a timing window where the ConfigMap is created before the webhook is registered, causing the namespace restriction to slip through. The admission pod eliminates this race. This is the standard pattern used by provider-aws, ACL, dns-service, cert-service, and all other Gardener extensions that have webhooks.

## Features

- **Any Helm chart** — Fluent Bit, Prometheus, nginx, your internal tools — anything that's a Helm chart
- **Runtime configuration** — addon config via Extension CR values, no custom builds needed per environment
- **OCI chart pull at runtime** — charts pulled from OCI registries on the seed, not baked into the binary
- **Embedded fallback** — optionally embed charts at build time for fully self-contained, air-gapped binaries
- **Managed seed aware** — skips seed addon deployment on managed seeds (parent seed handles it)
- **AWS infrastructure** — optional IAM policy attachment and VPC endpoint management per addon
- **Per-shoot overrides** — shoots can enable/disable addons or toggle features via `providerConfig` (planned)
- **GRM namespace provisioner** — in-process webhook ensures ManagedResources can deploy to any namespace

## Quick Start

```bash
# 1. Clone and build (binary ships bare -- no charts embedded)
git clone https://github.com/amendezsap/gardener-extension-shoot-addon-service
cd gardener-extension-shoot-addon-service
make release REGISTRY=registry.example.com/your-org

# 2. Deploy the Extension CR with your addon config
kubectl apply -f examples/extension.yaml   # edit values.addons first
```

The Extension CR's `values.addons` section defines which charts to pull from OCI registries and what values to apply. See [examples/extension.yaml](examples/extension.yaml) for the full format.

## Chart Sources

**Runtime (primary):** Specify OCI chart references in the Extension CR's `values.addons` section. Charts are pulled on the seed at runtime.

```yaml
values:
  addons:
    fluent-bit:
      chart:
        oci: oci://registry.example.com/charts/fluent-bit
        version: "0.56.0"
```

**Embedded (fallback):** For self-contained builds, pull charts into `charts/embedded/addons/` before building:

```bash
make pull-chart NAME=fluent-bit \
  OCI=oci://registry.example.com/charts/fluent-bit \
  VERSION=0.56.0
```

## Addon Configuration

Addons are configured in the Extension CR's `values.addons` section. The operator propagates this as a ConfigMap to each seed.

```yaml
values:
  addons:
    fluent-bit:
      enabled: true
      chart:
        oci: oci://registry.example.com/charts/fluent-bit
        version: "0.56.0"
      namespace: observability
      values:
        fullnameOverride: addon-fluent-bit
      aws:
        iamPolicies:
          - CloudWatchAgentServerPolicy
        vpcEndpoint:
          service: logs
```

For self-contained builds, addons can also be declared in `charts/embedded/addons/manifest.yaml` (see the embedded fallback method in [docs/usage.md](docs/usage.md)).

## Directory Structure

```
charts/embedded/addons/           # Empty by default — used only for embedded builds
├── manifest.yaml                 # declares what to deploy (embedded method only)
├── fluent-bit/                   # one directory per addon
│   ├── chart/                    # the Helm chart (pulled via make pull-chart)
│   └── values/                   # your values overlays
└── another-addon/
    ├── chart/
    └── values/
```

For runtime config (recommended), addon definitions live in the Extension CR -- this directory stays empty.

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
2. ConfigMap values from the Extension CR's `values.addons.<name>.values` section
3. Embedded `values/values.yaml` overlay (if using embedded method)
4. Embedded `values/values.<provider>.yaml` (e.g., `values.aws.yaml`, if using embedded method)
5. `shootValues` from the manifest
6. Image overrides from environment variables

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
