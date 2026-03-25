# Nginx Ingress Addon (Example)

This example shows how to add a third-party Helm chart as an addon. The nginx
ingress controller chart is pulled directly from the upstream Helm repository
at reconcile time -- no chart is embedded.

## Manifest snippet

```yaml
addons:
  - name: nginx-ingress
    chart:
      repo: https://kubernetes.github.io/ingress-nginx
      repoChart: ingress-nginx
      version: "4.12.1"
    namespace: ingress-nginx
    enabled: false
    image:
      valuesKey: controller.image
```

## Notes

- The `namespace` field overrides `defaultNamespace` for this addon only.
- The `image.valuesKey` tells the controller which Helm values key holds the
  container image reference, enabling automatic image registry rewriting when
  running in air-gapped environments.
- No values files are provided in this example. Add a `values/values.yaml` to
  customize the ingress controller configuration.
