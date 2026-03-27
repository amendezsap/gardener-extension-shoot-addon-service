# Deployment Guide

How to build, push, and deploy the extension to a Gardener landscape.

## Table of Contents

- [Prerequisites](#prerequisites)
- [Building Images](#building-images)
- [Pushing to a Registry](#pushing-to-a-registry)
- [Deploying to Gardener](#deploying-to-gardener)
- [Air-Gapped Environments](#air-gapped-environments)
- [Upgrading](#upgrading)
- [Uninstalling](#uninstalling)

## Prerequisites

- Go 1.24+
- [ko](https://ko.build/) for container image builds
- Helm 3.x for chart packaging
- Access to a Gardener landscape with the `operator.gardener.cloud/v1alpha1` Extension CRD
- An OCI-compatible container registry

## Building Images

The extension uses [ko](https://ko.build/) to build container images directly from Go source. No Dockerfiles needed. The binary ships bare -- `charts/embedded/addons/` is empty by default. The same binary works in every environment; addon configuration is provided at deploy time via the Extension CR.

```bash
# Build and push controller image
KO_DOCKER_REPO=registry.example.com/your-org/gardener-extension-shoot-addon-service \
  ko build --bare --tags v0.1.0 ./cmd/gardener-extension-shoot-addon-service

# Build and push admission webhook image
KO_DOCKER_REPO=registry.example.com/your-org/gardener-extension-admission-shoot-addon-service \
  ko build --bare --tags v0.1.0 ./cmd/gardener-extension-admission-shoot-addon-service
```

### Self-Contained Builds

If you need a fully self-contained binary with embedded charts (no runtime OCI pulls), populate `charts/embedded/addons/` before building:

```bash
# Pull charts into the embedded directory
make pull-chart NAME=fluent-bit \
  OCI=oci://registry.example.com/charts/fluent-bit \
  VERSION=0.56.0

# Build -- charts are compiled into the binary via go:embed
make release REGISTRY=registry.example.com/your-org TAG=v0.1.0
```

The extension uses a fallback chain: ConfigMap (runtime) -> embedded -> nothing. Self-contained builds still respect runtime ConfigMap config if present.

### Custom Base Image

By default, ko uses `gcr.io/distroless/static`. For air-gapped environments, override the base image:

```bash
export KO_DEFAULTBASEIMAGE=registry.example.com/distroless/static:nonroot
```

Or create a `.ko.yaml` file (not committed to the repo):

```yaml
defaultBaseImage: registry.example.com/distroless/static:nonroot
```

## Pushing to a Registry

### Helm Chart

```bash
# Package the Helm chart
helm package charts/gardener-extension-shoot-addon-service -d /tmp

# Push to OCI registry
helm push /tmp/gardener-extension-shoot-addon-service-0.1.0.tgz \
  oci://ghcr.io/your-org/charts
```

### Full Release

The `make release` target runs all steps: prepare addons, build images, push charts.

```bash
make release REGISTRY=ghcr.io/your-org TAG=v0.1.0
```

## Deploying to Gardener

### Operator Extension

Create an `operator.gardener.cloud/v1alpha1` Extension resource on the **runtime cluster**. The `values.addons` section defines which charts to pull and deploy -- this is the primary configuration method.

```yaml
apiVersion: operator.gardener.cloud/v1alpha1
kind: Extension
metadata:
  name: extension-shoot-addon-service
spec:
  deployment:
    extension:
      helm:
        ociRepository:
          ref: registry.example.com/your-org/charts/gardener-extension-shoot-addon-service:0.1.0
      injectGardenKubeconfig: true
      values:
        image:
          repository: registry.example.com/your-org/gardener-extension-shoot-addon-service
          tag: "0.1.0"
        admission:
          image:
            repository: registry.example.com/your-org/gardener-extension-admission-shoot-addon-service
            tag: "0.1.0"
        defaults:
          aws:
            vpcEndpoint:
              enabled: false
        grmNamespaces:
          - managed-resources
        # Addon configuration -- charts pulled from OCI at runtime.
        # addons.manifest is a multi-line string containing the AddonManifest.
        # addons.values holds per-addon values files keyed by name.
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
  resources:
    - kind: Extension
      type: shoot-addon-service
      primary: true
      autoEnable:
        - shoot
      lifecycle:
        reconcile: AfterKubeAPIServer
        delete: BeforeKubeAPIServer
        migrate: BeforeKubeAPIServer
```

Apply to the runtime cluster:

```bash
kubectl apply -f extension.yaml
```

The Gardener operator will:
1. Create a `ControllerRegistration` and `ControllerDeployment` in the virtual garden
2. The `values.addons` configuration is propagated as a ConfigMap to each seed
3. Gardenlet installs the extension on each seed via `ControllerInstallation`
4. The extension reads the ConfigMap, pulls charts from OCI, and reconciles all shoots
5. On managed seeds, the extension skips seed-targeted addon deployment (the parent handles it)

### Key Configuration

| Helm Value | Description | Default |
|---|---|---|
| `image.repository` | Controller image | (none) |
| `admission.image.repository` | Admission webhook image | (none) |
| `defaults.aws.vpcEndpoint.enabled` | Create VPC endpoints for shoots | `false` |
| `grmNamespaces` | Namespaces to inject into GRM config | `["observability"]` |
| `addons` | Addon definitions (chart refs, values, AWS config) | `{}` |
| `resources` | Controller pod resource requests/limits | See `values.yaml` |
| `addonImageOverrides` | Per-addon image overrides | `{}` |

### injectGardenKubeconfig

Setting `injectGardenKubeconfig: true` enables the extension to access the virtual garden API. This allows the controller to trigger immediate shoot reconciles after fixing stale GRM configs, instead of waiting for gardenlet's 60-minute resync.

The SeedAuthorizer grants extension service accounts gardenlet-equivalent permissions scoped to their seed's shoots. No custom RBAC is needed.

## Air-Gapped Environments

For environments without internet access:

1. **Mirror images** to your private registry:
   ```bash
   # Controller + admission images
   crane copy registry.example.com/gardener-extension-shoot-addon-service:0.1.0 \
     air-gapped-registry.example.com/gardener-extension-shoot-addon-service:0.1.0

   # Addon chart images (fluent-bit, etc.)
   crane copy cr.fluentbit.io/fluent/fluent-bit:3.2.6 \
     air-gapped-registry.example.com/fluent-bit:3.2.6
   ```

2. **Mirror addon charts** to your private OCI registry:
   ```bash
   # Copy addon Helm charts so the extension can pull them at runtime
   helm pull oci://registry.example.com/charts/fluent-bit --version 0.56.0
   helm push fluent-bit-0.56.0.tgz oci://air-gapped-registry.example.com/charts
   ```

3. **Build with private base image** (or use self-contained builds with embedded charts):
   ```bash
   export KO_DEFAULTBASEIMAGE=air-gapped-registry.example.com/distroless/static:nonroot
   make release REGISTRY=air-gapped-registry.example.com TAG=0.1.0
   ```

4. **Deploy with registry overrides** -- point OCI chart refs at your private registry:
   ```yaml
   values:
     image:
       repository: air-gapped-registry.example.com/gardener-extension-shoot-addon-service
     admission:
       image:
         repository: air-gapped-registry.example.com/gardener-extension-admission-shoot-addon-service
     addons:
       manifest: |
         apiVersion: addons.gardener.cloud/v1alpha1
         kind: AddonManifest
         defaultNamespace: managed-resources
         addons:
           - name: fluent-bit
             chart:
               oci: oci://air-gapped-registry.example.com/charts/fluent-bit
               version: "0.56.0"
             enabled: true
   ```

## Upgrading

In-place upgrade by updating the Extension resource:

```bash
# Update the Extension to the new version
kubectl apply -f extension.yaml  # with updated image tags and chart ref

# The operator updates the ControllerDeployment, gardenlet rolls out new pods
# on each seed. Shoots reconcile with the new extension version on the next
# gardenlet resync cycle (up to 60 minutes) or when triggered manually.
```

To trigger immediate rollout on a specific shoot:

```bash
kubectl annotate shoot <shoot-name> -n <namespace> \
  gardener.cloud/operation=reconcile --overwrite
```

## Uninstalling

1. Remove the extension from shoot specs first (if not using `autoEnable`):
   ```bash
   # Edit each shoot to remove the extension from spec.extensions
   kubectl edit shoot <shoot-name> -n <namespace>
   ```

2. Delete the operator Extension:
   ```bash
   kubectl delete extension.operator.gardener.cloud extension-shoot-addon-service
   ```

The operator will clean up the ControllerRegistration, ControllerDeployment, and all ControllerInstallations. The extension's Delete handler removes ManagedResources and AWS infrastructure (IAM policies, VPC endpoints) from each shoot.
