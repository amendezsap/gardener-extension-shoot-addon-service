# Usage Guide

This guide covers creating addon manifests, configuring chart sources, and deploying addons to shoots and seeds.

## Table of Contents

- [Addon Manifest](#addon-manifest)
- [Chart Sources](#chart-sources)
- [Deployment Targets](#deployment-targets)
- [Values Layering](#values-layering)
- [Template Variables](#template-variables)
- [Per-Shoot Configuration](#per-shoot-configuration)
- [AWS Infrastructure](#aws-infrastructure)
- [Image Overrides](#image-overrides)
- [GRM Namespace Provisioning](#grm-namespace-provisioning)

## Addon Manifest

The addon manifest (`addons/manifest.yaml`) declares which Helm charts the extension deploys. It is compiled into the binary at build time via `go:embed`.

```yaml
apiVersion: addons.gardener.cloud/v1alpha1
kind: AddonManifest

# Namespace where addon resources are deployed in each shoot/seed.
defaultNamespace: managed-resources

# AWS infrastructure applied to every shoot (optional).
globalAWS:
  iamPolicies:
    - CloudWatchAgentServerPolicy
  vpcEndpoints:
    - service: logs

addons:
  - name: fluent-bit
    chart:
      path: fluent-bit/chart
    valuesPath: fluent-bit/values
    enabled: true
    target: global
    managedResourceName: fluent-bit
    shootValues:
      fullnameOverride: fluent-bit
      env:
        - name: REGION
          value: "{{ .Region }}"
        - name: SEEDNAME
          value: "{{ .SeedName }}"
    image:
      valuesKey: image
```

### Addon Fields

| Field | Required | Description |
|---|---|---|
| `name` | Yes | Unique addon identifier |
| `chart.path` | Yes* | Path to chart relative to `addons/` (for embedded charts) |
| `chart.oci` | Yes* | OCI registry reference (e.g., `oci://ghcr.io/org/charts/mychart:1.0`) |
| `chart.repo` | Yes* | Helm repository URL |
| `chart.git` | Yes* | Git repository URL |
| `valuesPath` | No | Path to values directory relative to `addons/` |
| `enabled` | Yes | Default enabled state |
| `target` | No | Deployment target: `shoot` (default), `seed`, or `global` |
| `managedResourceName` | No | ManagedResource name (defaults to `addon-<name>`) |
| `shootValues` | No | Values merged into the chart at render time |
| `image` | No | Image override configuration |
| `imagePullSecrets` | No | List of pull secret names to inject |
| `namespace` | No | Override target namespace (defaults to `defaultNamespace`) |

*Exactly one chart source must be specified.

## Chart Sources

### Local (embedded)

Charts stored in `addons/<path>/` are compiled into the binary via `go:embed`.

```yaml
chart:
  path: fluent-bit/chart
```

Use `make prepare` to pull charts from remote sources into the local directory before building.

### OCI Registry

```yaml
chart:
  oci: oci://ghcr.io/fluent/helm-charts/fluent-bit
  version: "0.48.0"
```

### Helm Repository

```yaml
chart:
  repo: https://fluent.github.io/helm-charts
  repoChart: fluent-bit
  version: "0.48.0"
```

### Git Repository

```yaml
chart:
  git: https://github.com/fluent/helm-charts
  gitPath: charts/fluent-bit
  gitRef: main
```

## Deployment Targets

Each addon has a `target` field that controls where it is deployed:

| Target | Description | ManagedResource Class |
|---|---|---|
| `shoot` | Deploy to each shoot's worker nodes (default) | shoot |
| `seed` | Deploy once to the seed/runtime cluster | seed |
| `global` | Deploy to both shoots and the seed cluster | both |

### Shoot Target

Addons with `target: shoot` are deployed as shoot-class ManagedResources. The GRM in the shoot's control plane namespace reconciles them into the shoot cluster.

### Seed Target

Addons with `target: seed` are deployed as seed-class ManagedResources. The seed-level GRM (in the `garden` namespace) reconciles them onto the seed cluster itself. This is useful for infrastructure-level agents that need to run on seed nodes.

### Global Target

Addons with `target: global` are deployed to both. The same chart and values are used, with template variables resolving differently per context:

- **Shoot context:** `{{ .SeedName }}` = the seed managing the shoot
- **Seed context:** `{{ .SeedName }}` = the seed's own name (from `SEED_NAME` env var)

## Values Layering

Values are merged in order (later wins):

1. `addons/<valuesPath>/values.yaml` — base values
2. `addons/<valuesPath>/values.<provider>.yaml` — provider-specific (e.g., `values.aws.yaml`)
3. `addon.ShootValues` from `manifest.yaml` — per-addon shoot values
4. Image pull secrets from `addon.ImagePullSecrets`
5. Image overrides from environment variables

### Example

For addon `fluent-bit` on an AWS shoot:

```
values.yaml           → base config (service, parsers, general)
values.aws.yaml       → AWS-specific outputs (CloudWatch)
manifest.shootValues  → fullnameOverride, env vars (REGION, SEEDNAME)
env ADDON_FLUENT_BIT_IMAGE_REPOSITORY → image override
```

## Template Variables

The `shootValues` field supports template variable expansion:

| Variable | Description | Example Value |
|---|---|---|
| `{{ .Region }}` | Shoot's cloud provider region | `eu-west-1` |
| `{{ .SeedName }}` | Name of the seed managing the shoot | `my-seed` |
| `{{ .ShootName }}` | Shoot name | `my-shoot` |
| `{{ .ShootNamespace }}` | Shoot namespace in garden cluster | `garden-my-project` |
| `{{ .Project }}` | Gardener project name | `my-project` |
| `{{ .ControlNamespace }}` | Shoot control plane namespace on seed | `shoot--my-project--my-shoot` |

## Per-Shoot Configuration

Shoots can override extension defaults via `providerConfig` in the Shoot spec:

```yaml
apiVersion: core.gardener.cloud/v1beta1
kind: Shoot
spec:
  extensions:
    - type: shoot-addon-service
      providerConfig:
        apiVersion: shoot-addon-service.extensions.gardener.cloud/v1alpha1
        kind: Configuration
        aws:
          vpcEndpoint:
            enabled: true    # Override default VPC endpoint setting
        addons:
          fluent-bit:
            enabled: false   # Disable fluent-bit for this shoot
```

## AWS Infrastructure

The `globalAWS` section in the manifest defines AWS resources provisioned for every shoot:

### IAM Policies

Policies attached to the shoot's worker node IAM role:

```yaml
globalAWS:
  iamPolicies:
    - CloudWatchAgentServerPolicy
    - service-role/AmazonAPIGatewayPushToCloudWatchLogs
    - AmazonSSMManagedInstanceCore
```

### VPC Endpoints

Interface VPC endpoints created in the shoot's VPC. Required for air-gapped environments:

```yaml
globalAWS:
  vpcEndpoints:
    - service: logs    # Creates com.amazonaws.<region>.logs endpoint
```

VPC endpoint creation is controlled by `defaults.aws.vpcEndpoint.enabled` in the Helm chart values and can be overridden per-shoot via `providerConfig`.

## Image Overrides

Images can be overridden at deploy time via environment variables or Helm values:

```bash
# Environment variables (set in the controller deployment)
ADDON_FLUENT_BIT_IMAGE_REPOSITORY=my-registry.example.com/fluent-bit
ADDON_FLUENT_BIT_IMAGE_TAG=3.2.6
```

The addon must declare `image.valuesKey` to enable this:

```yaml
image:
  valuesKey: image                              # Key path in the chart's values.yaml
  defaultRepository: cr.fluentbit.io/fluent/fluent-bit  # Fallback if env not set
  defaultTag: "3.2.6"
```

## GRM Namespace Provisioning

The extension includes an admission webhook that injects custom namespaces into the GRM's `targetClientConnection.namespaces` list. This ensures the GRM watches namespaces where addon ManagedResources deploy their resources (e.g., `managed-resources`).

Configure the namespaces to inject via the Helm chart value:

```yaml
grmNamespaces:
  - managed-resources
  - my-custom-namespace
```

The webhook fires on GRM ConfigMap CREATE events and adds any missing namespaces to the existing list. If the namespaces field is absent, GRM already watches all namespaces and no injection is needed.
