# Usage Guide

This guide covers configuring addons, chart sources, deployment targets, and values layering.

## Table of Contents

- [Runtime Configuration (Recommended)](#runtime-configuration-recommended)
- [Addon Manifest (Embedded Fallback)](#addon-manifest-embedded-fallback)
- [Chart Sources](#chart-sources)
- [Deployment Targets](#deployment-targets)
- [Managed Seed Behavior](#managed-seed-behavior)
- [Values Layering](#values-layering)
- [Template Variables](#template-variables)
- [Per-Shoot Configuration](#per-shoot-configuration)
- [AWS Infrastructure](#aws-infrastructure)
- [Image Overrides](#image-overrides)
- [GRM Namespace Provisioning](#grm-namespace-provisioning)

## Runtime Configuration (Recommended)

The primary way to configure addons is through the Extension CR's `values.addons` section. The operator propagates this configuration as a ConfigMap to each seed. Charts are pulled from OCI registries at runtime -- no custom builds needed.

```yaml
# In the Extension CR applied to the runtime cluster
values:
  addons:
    manifest: |
      apiVersion: addons.gardener.cloud/v1alpha1
      kind: AddonManifest
      defaultNamespace: managed-resources
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
          managedResourceName: fluent-bit
          shootValues:
            fullnameOverride: fluent-bit
          image:
            valuesKey: image
    values:
      values.fluent-bit.yaml: |
        image:
          repository: registry.example.com/fluent-bit
          tag: "4.2.3"
      values.fluent-bit.aws.yaml: |
        config:
          outputs: |
            [OUTPUT]
                Name cloudwatch_logs
```

The extension reads this ConfigMap on each seed and uses it as the addon manifest. See [examples/extension.yaml](../examples/extension.yaml) for the full Extension CR format.

### Fallback Chain

The extension resolves addon configuration in this order:

1. **ConfigMap** (from Extension CR `values.addons`) -- primary, recommended
2. **Embedded charts** (from `charts/embedded/addons/`) -- fallback for self-contained builds
3. **Nothing** -- no addons deployed if neither source provides config

## Addon Manifest (Embedded Fallback)

For self-contained builds, addons can be declared in `charts/embedded/addons/manifest.yaml` and compiled into the binary via `go:embed`. This is the fallback method -- use it when you need a binary with zero runtime registry dependencies.

```yaml
apiVersion: addons.gardener.cloud/v1alpha1
kind: AddonManifest

defaultNamespace: managed-resources

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
    image:
      valuesKey: image
```

### Addon Fields

| Field | Required | Description |
|---|---|---|
| `name` | Yes | Unique addon identifier |
| `chart.path` | Yes* | Path to chart relative to `addons/` (for embedded charts) |
| `chart.oci` | Yes* | OCI registry reference (e.g., `oci://ghcr.io/org/charts/mychart`) |
| `chart.repo` | Yes* | Helm repository URL |
| `chart.git` | Yes* | Git repository URL |
| `valuesPath` | No | Path to values directory relative to `addons/` |
| `enabled` | Yes | Default enabled state |
| `target` | No | Deployment target: `shoot` (default), `seed`, or `global` |
| `managedResourceName` | No | ManagedResource name (defaults to addon name) |
| `shootValues` | No | Values merged into the chart at render time |
| `image` | No | Image override configuration |
| `imagePullSecrets` | No | List of pull secret names to inject |
| `namespace` | No | Override target namespace (defaults to `defaultNamespace`) |
| `keepObjectsOnRename` | No | Reserved for future use. Legacy MR cleanup always preserves resources (`keepObjects=true`). |
| `hooks` | No | Helm hook rendering configuration. See [docs/hooks.md](hooks.md) for details. |

*Exactly one chart source must be specified.

## Chart Sources

### OCI Registry (primary)

The recommended approach. Charts are pulled from OCI registries at runtime on the seed. Specify the chart reference in the Extension CR's `values.addons` section or in the embedded manifest.

```yaml
chart:
  oci: oci://registry.example.com/charts/fluent-bit
  version: "0.56.0"
```

### Local (embedded fallback)

Charts stored in `charts/embedded/addons/<path>/` are compiled into the binary via `go:embed`. Use `make prepare` to pull charts from remote sources into the local directory before building.

```yaml
chart:
  path: fluent-bit/chart
```

### Helm Repository

```yaml
chart:
  repo: https://charts.example.com/helm-charts
  repoChart: fluent-bit
  version: "0.48.0"
```

### Git Repository

```yaml
chart:
  git: https://github.com/example/helm-charts
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

## Managed Seed Behavior

On managed seeds, the extension skips seed-targeted addon deployment. The parent seed is responsible for deploying seed-level addons to managed seeds. This prevents duplicate deployments.

Addons with `target: shoot` are still deployed normally on managed seeds -- only `target: seed` and the seed portion of `target: global` are skipped.

## Values Layering

Values are merged in order (later wins):

1. Chart's built-in `values.yaml`
2. Base values -- `values.<addonName>.yaml` from ConfigMap (or embedded `values/values.yaml`)
3. Provider-specific values -- `values.<addonName>.<provider>.yaml` from ConfigMap (or embedded `values/values.<provider>.yaml`)
4. `shootValues` from the manifest
5. Image pull secrets from `addon.ImagePullSecrets`
6. Image overrides from environment variables

### Example (runtime config)

For addon `fluent-bit` on an AWS shoot configured via the Extension CR:

```
chart values.yaml                        -> chart defaults
values.fluent-bit.yaml from ConfigMap    -> base config from addons.values
values.fluent-bit.aws.yaml from ConfigMap -> AWS-specific outputs (CloudWatch)
shootValues                              -> fullnameOverride, env vars (REGION, SEEDNAME)
env ADDON_FLUENT_BIT_IMAGE_REPOSITORY    -> image override
```

### Example (embedded)

For addon `fluent-bit` on an AWS shoot using embedded charts:

```
chart values.yaml           -> chart defaults
values.yaml                 -> base config (service, parsers, general)
values.aws.yaml             -> AWS-specific outputs (CloudWatch)
manifest.shootValues        -> fullnameOverride, env vars (REGION, SEEDNAME)
env ADDON_FLUENT_BIT_IMAGE_REPOSITORY -> image override
```

## Template Variables

The `shootValues` field supports full [Go template](https://pkg.go.dev/text/template) syntax with [Sprig](https://masterminds.github.io/sprig/) string functions. This includes conditionals, pipelines, and string manipulation.

Available variables:

| Variable | Description | Example Value |
|---|---|---|
| `{{ .Region }}` | Cloud provider region | `eu-west-1` |
| `{{ .SeedName }}` | Name of the seed managing the shoot | `my-seed` |
| `{{ .ShootName }}` | Shoot name | `my-shoot` |
| `{{ .ShootNamespace }}` | Shoot namespace in garden cluster | `garden-my-project` |
| `{{ .Project }}` | Gardener project name | `my-project` |
| `{{ .ControlNamespace }}` | Shoot control plane namespace on seed | `shoot--my-project--my-shoot` |
| `{{ .ProviderType }}` | Cloud provider type (hyperscaler) | `aws`, `gcp`, `azure`, `openstack` |
| `{{ .ClusterRole }}` | Cluster role in the Gardener hierarchy | `runtime`, `managed-seed`, `shoot` |
| `{{ .ManagedKubernetesProvider }}` | Cloud-managed K8s distribution on the runtime | `GKE`, `EKS`, `AKS`, or empty |

### `{{ .ClusterRole }}` Values

Differentiates the three cluster roles in the Gardener hierarchy:

- `runtime` — the Gardener runtime cluster (where `gardener-operator` runs)
- `managed-seed` — a Gardener shoot promoted to a seed via `ManagedSeed`
- `shoot` — a regular workload shoot

### `{{ .ProviderType }}` Values

The cloud provider (hyperscaler) the cluster runs on. Sourced from:
- For shoots: `Shoot.Spec.Provider.Type`
- For runtime: `Seed.Spec.Provider.Type` of the seed registered for the runtime

Common values: `aws`, `gcp`, `azure`, `openstack`, `alicloud`. May be empty if the runtime cluster is operator-only and not registered as a seed (rare — write defensive templates if 100% reliability is required).

### `{{ .ManagedKubernetesProvider }}` Values

Set only when `ClusterRole=runtime` and the runtime is a cloud-provider-managed Kubernetes service. Detected via node labels.

Currently supported:
- `GKE` — Google Kubernetes Engine (label prefix `cloud.google.com/gke-`)
- `EKS` — Amazon Elastic Kubernetes Service (label prefix `eks.amazonaws.com/`)
- `AKS` — Azure Kubernetes Service (label prefix `kubernetes.azure.com/`)
- `OpenShift` — Red Hat OpenShift (label `node.openshift.io/os_id`)
- Empty — self-managed Kubernetes (including Gardener-provisioned shoots acting as seeds)

Additional managed Kubernetes services (OKE, DOKS, LKE, etc.) can be added on request. Open an issue if you need detection for a service not listed above.

#### Not currently detected

The following are noted as possible future enhancements but **not currently supported**:

- **OpenStack-based managed Kubernetes services** — services like OTC CCE, SAP Converged Cloud, or Yandex Managed Kubernetes that run on OpenStack infrastructure. Note that OpenStack itself is an IaaS, not a managed Kubernetes service — clusters provisioned by Gardener on OpenStack VMs correctly report `ProviderType=openstack` and `ManagedKubernetesProvider=""`. Detection would only apply to clusters where the cloud provider also runs the Kubernetes control plane.

### Example: Provider-aware addon configuration

```yaml
shootValues:
  myAddon:
    # Tell the addon what kind of cluster it's installed on
    clusterFlavor: |-
      {{- if eq .ClusterRole "runtime" }}{{ .ManagedKubernetesProvider }}{{- else }}Kubernetes{{- end }}
    # Pick a provider-specific backend
    cloudProvider: "{{ .ProviderType }}"
```

Evaluates to:

| Cluster | `clusterFlavor` | `cloudProvider` |
|---|---|---|
| GKE runtime | `GKE` | `gcp` |
| EKS runtime | `EKS` | `aws` |
| Managed seed (any cloud) | `Kubernetes` | `aws` / `gcp` / etc. |
| Regular shoot | `Kubernetes` | `aws` / `gcp` / etc. |

### Sprig String Functions

All [Sprig](https://masterminds.github.io/sprig/) text functions are available except security-sensitive ones (`env`, `expandenv`, crypto generators). Commonly useful:

```yaml
shootValues:
  myAddon:
    # String manipulation
    upperProvider: "{{ .ProviderType | upper }}"            # AWS
    shortRole: "{{ trimPrefix "managed-" .ClusterRole }}"   # seed
    # Defaults for empty values
    k8sService: "{{ .ManagedKubernetesProvider | default "Kubernetes" }}"
    # String testing
    isSeed: "{{- if contains "seed" .ClusterRole }}true{{- else }}false{{- end }}"
```

### Security

Template execution is sandboxed:
- **Blocked functions**: `env`, `expandenv`, `genPrivateKey`, `genCA`, and other crypto/secret functions are removed
- **Timeout**: Templates must complete within 5 seconds
- **Output size limit**: Rendered output capped at 1MB
- **Passthrough on error**: If a template fails to parse or execute, the original string is returned unchanged

## Per-Shoot Configuration

Shoots can override extension defaults via `providerConfig` in the Shoot spec. Per-shoot overrides take **highest priority** — they override the global manifest setting.

### Disabling an Addon Per-Shoot

```yaml
apiVersion: core.gardener.cloud/v1beta1
kind: Shoot
spec:
  extensions:
    - type: shoot-addon-service
      providerConfig:
        apiVersion: shoot-addon-service.extensions.gardener.cloud/v1alpha1
        kind: Configuration
        addons:
          my-addon:
            enabled: false   # Disable on this shoot only
```

When an addon is disabled per-shoot:
- The addon's ManagedResource is deleted from the shoot
- Delete hooks run (e.g., connector deregistration)
- All addon resources are removed from the shoot
- Other shoots are unaffected — the addon remains active globally

When the override is removed or set back to `enabled: true`, the addon is re-deployed as a fresh install (hook Jobs run again).

### Enabling a Globally Disabled Addon Per-Shoot

```yaml
# In the manifest: my-addon has enabled: false (disabled globally)
# In the Shoot spec: override enables it on this shoot only
providerConfig:
  addons:
    my-addon:
      enabled: true   # Override global disable for this shoot
```

### Overriding Values Per-Shoot

```yaml
providerConfig:
  addons:
    my-addon:
      valuesOverride: |
        config:
          logLevel: debug
      valuesMode: merge   # "merge" (default) or "override"
```

- **merge** (default): deep-merge with existing values. Only specified keys change.
- **override**: full replacement. All previous values are discarded.

### Priority Order

Values are layered (later wins):

1. Base values from ConfigMap (`values.<addon>.yaml`)
2. Provider-specific values (`values.<addon>.<provider>.yaml`)
3. `shootValues` from the manifest (with template expansion)
4. Image overrides from environment variables
5. **Per-shoot `valuesOverride`** from providerConfig (highest priority)

### AWS Overrides

```yaml
providerConfig:
  apiVersion: shoot-addon-service.extensions.gardener.cloud/v1alpha1
  kind: Configuration
  aws:
    vpcEndpoint:
      enabled: true    # Override default VPC endpoint setting
```

### Limitations

- Per-shoot overrides only affect **shoot-targeted** addons. Seed addons are shared infrastructure and cannot be disabled per-shoot.
- Disabling an addon per-shoot triggers the full removal lifecycle including delete hooks. This is intentional — partial removal (resources without hooks) is not supported.

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
