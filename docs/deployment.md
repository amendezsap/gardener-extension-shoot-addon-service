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

- Go 1.25+
- [ko](https://ko.build/) for container image builds
- Helm 3.x for chart packaging
- Access to a Gardener landscape with the `operator.gardener.cloud/v1alpha1` Extension CRD
- An OCI-compatible container registry

## Building Images

The extension uses [ko](https://ko.build/) to build container images directly from Go source. No Dockerfiles needed.

```bash
# Build and push controller image
KO_DOCKER_REPO=ghcr.io/your-org/gardener-extension-shoot-addon-service \
  ko build --bare --tags v0.1.0 ./cmd/gardener-extension-shoot-addon-service

# Build and push admission webhook image
KO_DOCKER_REPO=ghcr.io/your-org/gardener-extension-admission-shoot-addon-service \
  ko build --bare --tags v0.1.0 ./cmd/gardener-extension-admission-shoot-addon-service
```

### Custom Base Image

By default, ko uses `gcr.io/distroless/static`. For air-gapped environments, override the base image:

```bash
export KO_DEFAULTBASEIMAGE=my-registry.example.com/distroless/static:nonroot
```

Or create a `.ko.yaml` file (not committed to the repo):

```yaml
defaultBaseImage: my-registry.example.com/distroless/static:nonroot
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

Create an `operator.gardener.cloud/v1alpha1` Extension resource on the **runtime cluster**:

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
          ref: ghcr.io/your-org/charts/gardener-extension-shoot-addon-service:0.1.0
      injectGardenKubeconfig: true
      values:
        image:
          repository: ghcr.io/your-org/gardener-extension-shoot-addon-service
          tag: "0.1.0"
        admission:
          image:
            repository: ghcr.io/your-org/gardener-extension-admission-shoot-addon-service
            tag: "0.1.0"
        defaults:
          aws:
            vpcEndpoint:
              enabled: false
        grmNamespaces:
          - managed-resources
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
2. Gardenlet installs the extension on each seed via `ControllerInstallation`
3. The extension reconciles all shoots automatically

### Key Configuration

| Helm Value | Description | Default |
|---|---|---|
| `image.repository` | Controller image | `ghcr.io/amendezsap/gardener-extension-shoot-addon-service` |
| `admission.image.repository` | Admission webhook image | `ghcr.io/amendezsap/gardener-extension-admission-shoot-addon-service` |
| `defaults.aws.vpcEndpoint.enabled` | Create VPC endpoints for shoots | `false` |
| `grmNamespaces` | Namespaces to inject into GRM config | `["observability"]` |
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
   crane copy ghcr.io/amendezsap/gardener-extension-shoot-addon-service:0.1.0 \
     my-registry.example.com/gardener-extension-shoot-addon-service:0.1.0

   # Addon images (fluent-bit, etc.)
   crane copy cr.fluentbit.io/fluent/fluent-bit:3.2.6 \
     my-registry.example.com/fluent-bit:3.2.6
   ```

2. **Update addon values** to reference your registry:
   ```yaml
   # In addons/fluent-bit/values/values.yaml
   image:
     repository: my-registry.example.com/fluent-bit
   ```

3. **Build with private base image**:
   ```bash
   export KO_DEFAULTBASEIMAGE=my-registry.example.com/distroless/static:nonroot
   make release REGISTRY=my-registry.example.com TAG=0.1.0
   ```

4. **Deploy with registry overrides**:
   ```yaml
   values:
     image:
       repository: my-registry.example.com/gardener-extension-shoot-addon-service
     admission:
       image:
         repository: my-registry.example.com/gardener-extension-admission-shoot-addon-service
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
