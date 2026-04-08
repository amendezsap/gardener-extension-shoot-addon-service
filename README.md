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
│       ├── Injects required namespaces into targetClientConnection.namespaces at CREATE
│       └── Enables ManagedResources to deploy to custom namespaces
```

**Controller pod** (`cmd/gardener-extension-shoot-addon-service/`): The main reconciliation loop. Watches Extension resources, loads addon config from the seed ConfigMap (falling back to embedded charts), pulls charts from OCI registries, renders them, creates ManagedResources, and manages AWS infrastructure. Participates in leader election like all Gardener extension controllers. On managed seeds, the extension skips seed-targeted addon deployment (the parent seed handles it).

**Admission pod** (`cmd/gardener-extension-admission-shoot-addon-service/`): A dedicated webhook server that intercepts GRM ConfigMap creation and injects required namespaces into the target namespace list. Runs independently of the controller pod with no leader election dependency, so it is always ready to intercept ConfigMap creates even during controller rollouts or restarts.

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

## Addon Configuration

Addons are configured in the Extension CR's `values.addons` section. This creates a ConfigMap on each seed with the manifest and values. The extension reads the ConfigMap and pulls charts from OCI registries at runtime.

```yaml
values:
  addons:
    manifest: |
      apiVersion: addons.gardener.cloud/v1alpha1
      kind: AddonManifest
      defaultNamespace: observability
      globalAWS:
        iamPolicies:
          - CloudWatchAgentServerPolicy
        vpcEndpoints:
          - service: logs
      addons:
        - name: fluent-bit
          chart:
            oci: oci://registry.example.com/charts/fluent-bit
            version: "0.56.0"
          enabled: true
          target: global
    values:
      values.fluent-bit.yaml: |
        image:
          repository: registry.example.com/fluent-bit
          tag: "4.2.3"
```

For self-contained builds, addons can also be embedded at build time — see the [usage guide](docs/usage.md) for the embedded fallback method.

## Per-Shoot Configuration

### Disable individual addons

Use `providerConfig` to disable specific addons on a shoot without affecting others:

```yaml
spec:
  extensions:
  - type: shoot-addon-service
    providerConfig:
      addons:
        fluent-bit:
          enabled: false          # disable on this shoot
        my-addon:
          enabled: true           # keep enabled on this shoot
      aws:
        vpcEndpoint:
          enabled: true           # use VPC endpoint instead of NAT
```

The extension still runs on the shoot — it just skips the disabled addons. AWS infrastructure (IAM policies, VPC endpoints) remains in place.

### Override addon values per shoot

For debugging or per-shoot tuning, override specific values. Merge mode (default) only changes the keys you specify:

```yaml
providerConfig:
  addons:
    fluent-bit:
      valuesOverride: |
        config:
          outputs: |
            [OUTPUT]
                Name stdout
                Match *
```

For a full replacement of all values, use override mode:

```yaml
providerConfig:
  addons:
    fluent-bit:
      valuesMode: override
      valuesOverride: |
        kind: DaemonSet
        image:
          repository: debug-image
          tag: test
```

### Disable the extension entirely (full teardown)

Setting `disabled: true` triggers the extension's Delete handler, which performs a complete teardown:
- Deletes all addon ManagedResources (fluent-bit, container-report, etc.)
- Detaches IAM policies from the shoot's node role
- Deletes VPC endpoints (only if created by the extension and no other shoots use them)

```yaml
spec:
  extensions:
  - type: shoot-addon-service
    disabled: true
```

This is different from setting `enabled: false` on individual addons — `disabled: true` removes all AWS infrastructure and addon resources. Removing `disabled: true` and reconciling restores everything.

**Note:** With `autoEnable: shoot` set on the Extension CR, the extension is automatically added to all shoots. To exclude a specific shoot, you must explicitly add `disabled: true` — you cannot remove it from the shoot spec (gardenlet re-adds it).

## Multi-Provider Support

The extension detects the cloud provider from `Shoot.Spec.Provider.Type` and applies provider-specific behavior automatically. Chart deployment works on all providers. IAM management is provider-specific.

### Provider Detection

Each shoot's provider type (`aws`, `gcp`, `openstack`, etc.) is read from the Shoot spec. The extension:
- Deploys addon charts to all shoots regardless of provider
- Applies provider-specific values overlays (`values.<addon>.<provider>.yaml`)
- Manages provider-specific IAM configuration only for supported providers
- Skips unsupported provider features silently

### Provider-Specific Values Overlays

Addon values are layered by provider. For a Fluent Bit addon on a GCP shoot, the extension merges:
1. `values.fluent-bit.yaml` (base values)
2. `values.fluent-bit.gcp.yaml` (GCP-specific outputs, e.g., Stackdriver)

On AWS, it would merge `values.fluent-bit.aws.yaml` instead (e.g., CloudWatch outputs). This allows a single addon definition to target multiple providers with different backend configurations.

### Global Provider Config

Provider-specific infrastructure (IAM policies, VPC endpoints, IAM role bindings) is configured in the manifest's `globalAWS` and `globalGCP` sections. These apply to all shoots on that provider regardless of which addons are enabled.

## AWS Features (provider-aws only)

**Global IAM Policies** -- attached to the shoot's node role:
```yaml
globalAWS:
  iamPolicies:
    - CloudWatchAgentServerPolicy
    - AmazonSSMManagedInstanceCore
```

**Global VPC Endpoints** -- created in the shoot's VPC with the Gardener node security group:
```yaml
globalAWS:
  vpcEndpoints:
    - service: logs    # → com.amazonaws.<region>.logs
```

VPC endpoints support shared VPCs (tag-based tracking prevents premature deletion) and are configurable per-shoot via `providerConfig`.

IAM policies removed from `globalAWS.iamPolicies` are automatically detached on the next reconcile (stale policy detection via ProviderStatus).

## GCP Features (provider-gcp only)

**Global IAM Role Bindings** -- bound to the shoot's node service account at the project level:
```yaml
globalGCP:
  iamRoles:
    - roles/logging.logWriter
    - roles/monitoring.metricWriter
```

The node service account email is extracted from the Infrastructure CR status (`serviceAccountEmail`). IAM role bindings use `serviceAccount:<email>` member format and are managed idempotently with etag-based conflict retry.

Roles removed from `globalGCP.iamRoles` are automatically unbound on the next reconcile (stale role detection via ProviderStatus).

### Example: Multi-Provider Manifest

```yaml
apiVersion: addons.gardener.cloud/v1alpha1
kind: AddonManifest
defaultNamespace: observability
globalAWS:
  iamPolicies:
    - CloudWatchAgentServerPolicy
  vpcEndpoints:
    - service: logs
globalGCP:
  iamRoles:
    - roles/logging.logWriter
    - roles/monitoring.metricWriter
addons:
  - name: fluent-bit
    chart:
      oci: oci://registry.example.com/charts/fluent-bit
      version: "0.56.0"
    valuesPath: fluent-bit/values
    enabled: true
```

With provider-specific values files:
- `values.fluent-bit.yaml` -- base config (shared across all providers)
- `values.fluent-bit.aws.yaml` -- CloudWatch Logs output
- `values.fluent-bit.gcp.yaml` -- Stackdriver/Cloud Logging output

## Values Layering

For each addon, values are merged in order (last wins):

1. Chart's built-in `values.yaml` (from OCI pull or embedded chart)
2. Base values — `values.<addonName>.yaml` from ConfigMap (or embedded `values/values.yaml`)
3. Provider-specific values — `values.<addonName>.<provider>.yaml` from ConfigMap (or embedded)
4. `shootValues` from the addon manifest
5. Image overrides from environment variables

## Prerequisites

- Go 1.24+
- [ko](https://ko.build/) — container image builder for Go (`go install github.com/google/ko@latest`)
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
