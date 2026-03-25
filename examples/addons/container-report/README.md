# Container Report Addon (Example)

This is an **example** addon for generating container inventory reports on shoot
clusters. It deploys a CronJob that periodically lists all running container
images and reports them to a central endpoint.

## Usage

1. Copy this directory into the embedded addons tree:

   ```
   cp -r examples/addons/container-report charts/embedded/addons/container-report
   ```

2. Reference it in your `manifest.yaml`:

   ```yaml
   addons:
     - name: container-report
       chart:
         path: container-report/chart
       valuesPath: container-report/values
       enabled: true
   ```

3. Rebuild the extension image.

## Values

- `values/values.yaml` -- CronJob schedule and image configuration
