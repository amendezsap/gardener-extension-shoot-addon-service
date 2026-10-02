{{- /*
Name of the chart-rendered image-pull secret. Referenced by
secret-image-pull.yaml (what it creates) and by both deployments (what they
append to .spec.template.spec.imagePullSecrets when a private registry is used).
*/ -}}
{{- define "shoot-addon-service.imagePullSecretName" -}}
gardener-extension-shoot-addon-service-image-pull
{{- end -}}
