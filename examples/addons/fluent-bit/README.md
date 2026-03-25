# Fluent Bit Addon (Example)

This is an **example** addon configuration for deploying Fluent Bit to shoot
clusters via gardener-extension-shoot-addon-service.

## Usage

1. Copy this directory into the embedded addons tree:

   ```
   cp -r examples/addons/fluent-bit charts/embedded/addons/fluent-bit
   ```

2. Reference it in your `manifest.yaml`:

   ```yaml
   addons:
     - name: fluent-bit
       chart:
         oci: oci://ghcr.io/fluent/helm-charts/fluent-bit
         version: "0.56.0"
       valuesPath: fluent-bit/values
       enabled: true
   ```

3. Rebuild the extension image so the values files are included via `go:embed`.

## Chart source

The Helm chart is pulled from the upstream Fluent project:

- **OCI**: `oci://ghcr.io/fluent/helm-charts/fluent-bit`
- **Git**: `https://github.com/fluent/helm-charts` (path: `charts/fluent-bit`)

The chart itself is NOT embedded -- only the values overlay files in `values/`
are embedded. The chart is fetched at reconcile time from the source specified
in the addon manifest.

## Values files

- `values/values.yaml` -- base values (volume mounts, common config)
- `values/values.aws.yaml` -- AWS-specific CloudWatch output configuration

Values are merged in order: base first, then cloud-provider-specific overlays.
