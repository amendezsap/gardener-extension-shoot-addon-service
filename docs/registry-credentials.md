# Registry Credentials & Image Pull Configuration

How private registry authentication works for addons deployed to Gardener shoots
via `gardener-extension-shoot-addon-service`.

## Table of Contents

- [Overview](#overview)
- [Why Gardener Does Not Auto-Inject imagePullSecrets](#why-gardener-does-not-auto-inject-imagepullsecrets)
- [Pattern A: Node-Level containerd Config (Cloud Provider IAM)](#pattern-a-node-level-containerd-config-cloud-provider-iam)
- [Pattern B: Extension-Deployed Registry Secrets (seedSecretRef)](#pattern-b-extension-deployed-registry-secrets-seedsecretref)
- [Pattern C: Kyverno Mutating Webhook](#pattern-c-kyverno-mutating-webhook)
- [Manifest Configuration Reference](#manifest-configuration-reference)
- [Full Manifest Example](#full-manifest-example)
- [Credential Flow Diagram](#credential-flow-diagram)
- [Security Best Practices](#security-best-practices)
- [FAQ](#faq)

---

## Overview

Pods running in a Gardener shoot need credentials to pull images from private
registries. There are three patterns for providing those credentials, each
suited to different environments:

| Pattern | Mechanism | When to Use |
|---|---|---|
| **A -- Node-level containerd** | IAM instance profile / workload identity / host-level registry mirror | ECR, GCR, ACR, or air-gapped environments with node mirrors |
| **B -- Extension-deployed Secrets** | `seedSecretRef` in manifest -> `imagePullSecrets` on Pod | Any registry requiring user/password or token auth (private OCI registries, Artifactory, Docker Hub, etc.) |
| **C -- Kyverno webhook** | MutatingWebhookConfiguration injects `imagePullSecrets` at admission time | Large multi-team clusters where central policy enforcement is preferred |

Choose the pattern that matches your registry and environment. Pattern A is the
simplest when your cloud provider supports it. Pattern B works with any registry
that accepts static credentials.

---

## Why Gardener Does Not Auto-Inject imagePullSecrets

A common question: "Why doesn't Gardener just inject `imagePullSecrets` into
every Pod automatically?"

The answer lies in how ManagedResources work:

1. The extension builds a bundle of Kubernetes manifests (Deployments, DaemonSets,
   Secrets, RBAC, etc.) and wraps them in a **ManagedResource** object.
2. The **Gardener Resource Manager (GRM)** running on the seed applies those
   manifests **verbatim** to the shoot's API server via the shoot's admin
   kubeconfig.
3. GRM has **no injection layer**. It does not mutate the manifests. Whatever
   the extension puts into the ManagedResource bundle is exactly what appears
   in the shoot.

This means:

- If a Pod spec does not include `imagePullSecrets`, GRM will not add any.
- If a Secret of type `kubernetes.io/dockerconfigjson` is not in the bundle,
  it will not exist in the shoot.
- The extension is responsible for including both the Secret and the Pod-level
  reference to that Secret.

This design is intentional -- it keeps GRM simple, predictable, and auditable.
The extension has full control over what gets deployed.

---

## Pattern A: Node-Level containerd Config (Cloud Provider IAM)

### How it works

1. Gardener provisions shoot worker nodes with an **IAM instance profile** (AWS),
   **Workload Identity** (GCP), or **Managed Identity** (Azure) that includes
   permissions to pull from the cloud provider's container registry.
2. The kubelet on each node uses the **credential provider plugin** (or
   containerd's built-in registry resolver) to obtain short-lived tokens
   automatically.
3. Pods do not need `imagePullSecrets` at all -- the node handles auth
   transparently.

### Cloud provider examples

| Provider | Registry | IAM Mechanism |
|---|---|---|
| AWS | ECR | Instance profile with `ecr:GetAuthorizationToken`, `ecr:BatchGetImage`, `ecr:GetDownloadUrlForLayer` |
| GCP | GCR / Artifact Registry | Workload Identity with `roles/artifactregistry.reader` |
| Azure | ACR | Managed Identity with `AcrPull` role |

### When to use

- All images are in your cloud provider's native registry.
- You do not want to manage per-registry Secrets.

### Limitations

- Only works for registries that support instance-level or workload-identity
  auth.
- Does not work for third-party registries that require explicit
  username/password credentials.
- Tightly couples image pull auth to the cloud provider's IAM model.

### Example manifest (no imagePullSecrets needed)

When all images come from a registry with node-level auth, the manifest does
not need `registrySecrets` or `imagePullSecrets`:

```yaml
addons:
  - name: fluent-bit
    chart:
      path: fluent-bit/chart
    image:
      valuesKey: image
    # No imagePullSecrets -- nodes have IAM roles for the registry

  - name: container-report
    chart:
      path: container-report/chart
    image:
      valuesKey: image
    # No imagePullSecrets -- nodes have IAM roles for the registry
```

---

## Pattern B: Extension-Deployed Registry Secrets (seedSecretRef)

Use this pattern when your addon images live in a registry that requires
explicit credentials (private OCI registries, Artifactory, Docker Hub private
repos, etc.).

### How it works

The credential flow has four stages:

1. **Platform admin creates a Secret on the seed cluster.** This Secret
   contains the `dockerconfigjson` for the target registry. It lives in the
   extension's namespace on the seed (e.g., `extension-shoot-addon-service`).

2. **Extension reads the Secret at reconciliation time.** When the extension
   processes a shoot, it looks up each `seedSecretRef` declared in the manifest
   and reads the corresponding Secret from the seed.

3. **Extension includes the Secret in the ManagedResource bundle.** The
   Secret is added to the bundle as a `kubernetes.io/dockerconfigjson` Secret
   targeting the shoot namespace specified in the manifest.

4. **GRM deploys the Secret to the shoot.** The Secret appears in the shoot
   cluster, and Pods reference it via `imagePullSecrets`. Kubelet uses it to
   authenticate with the registry.

### Step-by-step setup

#### 1. Create the registry Secret on the seed

```bash
# On the seed cluster, in the extension's namespace
kubectl create secret docker-registry my-registry-creds \
  --namespace=extension-shoot-addon-service \
  --docker-server=registry.example.com \
  --docker-username=svc-addon-puller \
  --docker-password='<password>' \
  --docker-email=addon-puller@example.com
```

Label the Secret so operators can identify it:

```bash
kubectl label secret my-registry-creds \
  --namespace=extension-shoot-addon-service \
  app.kubernetes.io/managed-by=shoot-addon-service \
  registry=my-registry
```

#### 2. Reference the Secret in the addon manifest

In `manifest.yaml`, declare the Secret under `registrySecrets` and reference
it from each addon that needs it:

```yaml
apiVersion: addons.gardener.cloud/v1alpha1
kind: AddonManifest

defaultNamespace: managed-resources

registrySecrets:
  - name: my-registry
    seedSecretRef:
      name: my-registry-creds
      namespace: extension-shoot-addon-service

addons:
  - name: fluent-bit
    chart:
      path: fluent-bit/chart
    valuesPath: fluent-bit/values
    enabled: true
    image:
      valuesKey: image
    imagePullSecrets:
      - my-registry

  - name: container-report
    chart:
      path: container-report/chart
    valuesPath: container-report/values
    enabled: true
    image:
      valuesKey: image
    imagePullSecrets:
      - my-registry
```

#### 3. Verify in the shoot

After the extension reconciles:

```bash
# Check that the Secret was deployed to the shoot
kubectl get secret my-registry \
  --namespace=managed-resources \
  -o jsonpath='{.type}'
# Expected: kubernetes.io/dockerconfigjson

# Check that the DaemonSet references it
kubectl get daemonset addon-fluent-bit \
  --namespace=managed-resources \
  -o jsonpath='{.spec.template.spec.imagePullSecrets}'
# Expected: [{"name":"my-registry"}]
```

### Security model

The key security property of this pattern is that **actual credentials never
appear in git, in the manifest file, or in the extension binary**. The manifest
contains only a **pointer** (the `seedSecretRef` name and namespace) to a Secret
that lives on the seed cluster.

| Layer | What is stored |
|---|---|
| Git / manifest.yaml | Secret *name* and *namespace* (pointer only) |
| Extension binary | No credentials -- reads Secrets at runtime |
| Seed cluster | The actual `dockerconfigjson` Secret |
| Shoot cluster | A copy of the `dockerconfigjson` Secret, deployed by GRM |

An attacker who compromises the git repo or the extension binary gains no
registry credentials. They would need access to the seed cluster's etcd or
API server to read the actual Secret.

---

## Pattern C: Kyverno Mutating Webhook

Kyverno can inject `imagePullSecrets` into every Pod at admission time using a
`ClusterPolicy` with a mutate rule. This is useful when:

- You have many teams deploying workloads and want central control over registry
  credentials.
- You do not want each team to manage their own `imagePullSecrets`.

See the Gardener community example:
[Kyverno imagePullSecrets injection](https://github.com/gardener/gardener/blob/master/docs/extensions/registry-cache.md)

The general approach:

1. Deploy Kyverno to the shoot (can itself be an addon).
2. Create a `ClusterPolicy` that matches all Pods and adds `imagePullSecrets`.
3. Create the target `docker-registry` Secret in each namespace.

This pattern adds complexity (Kyverno is another component to manage) and
latency (webhook call on every Pod creation). For most addon use cases,
Pattern A or Pattern B is simpler.

---

## Manifest Configuration Reference

### registrySecrets

Top-level array declaring registry credentials the extension should read from
the seed and deploy to the shoot.

```yaml
registrySecrets:
  - name: <shoot-secret-name>        # Name of the Secret created in the shoot
    seedSecretRef:
      name: <seed-secret-name>        # Name of the Secret on the seed
      namespace: <seed-namespace>     # Namespace of the Secret on the seed
```

- `name`: The name the Secret will have in the shoot cluster. This is the name
  addons reference in their `imagePullSecrets` list.
- `seedSecretRef.name`: The name of the existing `docker-registry` Secret on
  the seed cluster.
- `seedSecretRef.namespace`: The namespace where the seed Secret lives.

### Per-addon imagePullSecrets

Each addon can reference one or more registry Secrets by name:

```yaml
addons:
  - name: my-addon
    imagePullSecrets:
      - my-registry      # References registrySecrets[].name
      - dockerhub-mirror  # Can reference multiple registries
```

### Multiple addons sharing the same registry Secret

If multiple addons pull from the same registry, declare the registry Secret once
and reference it from each addon:

```yaml
registrySecrets:
  - name: my-registry
    seedSecretRef:
      name: my-registry-creds
      namespace: extension-shoot-addon-service

addons:
  - name: fluent-bit
    imagePullSecrets:
      - my-registry

  - name: container-report
    imagePullSecrets:
      - my-registry
```

The extension creates exactly one `my-registry` Secret in the shoot, and both
DaemonSet/CronJob reference it.

### Multiple registries with different Secrets

If different addons pull from different registries, declare multiple registry
Secrets:

```yaml
registrySecrets:
  - name: my-registry
    seedSecretRef:
      name: my-registry-creds
      namespace: extension-shoot-addon-service

  - name: dockerhub-creds
    seedSecretRef:
      name: dockerhub-mirror-creds
      namespace: extension-shoot-addon-service

addons:
  - name: fluent-bit
    imagePullSecrets:
      - my-registry

  - name: metrics-exporter
    imagePullSecrets:
      - dockerhub-creds
```

---

## Full Manifest Example

A complete manifest using private registry credentials for both fluent-bit and
container-report:

```yaml
apiVersion: addons.gardener.cloud/v1alpha1
kind: AddonManifest

defaultNamespace: managed-resources

# --- Registry Secrets ---
# The extension reads these from the seed and deploys them to the shoot.
# Actual credentials are NOT in this file -- only pointers to seed Secrets.
registrySecrets:
  - name: my-registry
    seedSecretRef:
      name: my-registry-creds
      namespace: extension-shoot-addon-service

# --- Addons ---
addons:
  - name: fluent-bit
    chart:
      path: fluent-bit/chart
    valuesPath: fluent-bit/values
    enabled: true
    shootValues:
      fullnameOverride: addon-fluent-bit
    image:
      valuesKey: image
    imagePullSecrets:
      - my-registry
    aws:
      iamPolicies:
        - CloudWatchAgentServerPolicy
        - service-role/AmazonAPIGatewayPushToCloudWatchLogs
      vpcEndpoint:
        service: logs

  - name: container-report
    chart:
      path: container-report/chart
    valuesPath: container-report/values
    enabled: true
    image:
      valuesKey: image
    imagePullSecrets:
      - my-registry
```

The fluent-bit values file (`fluent-bit/values/values.yaml`) already has an
`imagePullSecrets: []` field. When the extension detects `imagePullSecrets` in
the manifest, it populates this values field automatically:

```yaml
# Injected by the extension at reconciliation time
imagePullSecrets:
  - name: my-registry
```

---

## Credential Flow Diagram

The complete lifecycle of registry credentials from build time through runtime:

```
BUILD TIME (CI/CD)
==================

  Developer pushes code
        |
        v
  CI pipeline runs
        |
        +-- helm registry login registry.example.com
        |     (uses CI service account token -- short-lived)
        |
        +-- addon-prepare: pulls charts, embeds in binary
        |
        +-- docker build / docker push
        |     (pushes extension image to registry)
        |
        v
  Extension image in registry


DEPLOY TIME (Seed)
==================

  Platform admin creates seed Secret (one-time setup)
        |
        v
  kubectl create secret docker-registry my-registry-creds \
    --namespace=extension-shoot-addon-service \
    --docker-server=registry.example.com \
    --docker-username=svc-addon-puller \
    --docker-password='...'
        |
        v
  Extension deployed to seed
  (reads manifest.yaml baked into binary)


RECONCILIATION TIME (per shoot)
===============================

  Shoot created/updated
        |
        v
  Extension controller reconciles
        |
        +-- Reads seedSecretRef "my-registry-creds"
        |   from namespace "extension-shoot-addon-service"
        |
        +-- Builds ManagedResource bundle:
        |     - Secret "my-registry" (type: dockerconfigjson)
        |     - DaemonSet "addon-fluent-bit" (imagePullSecrets: my-registry)
        |     - CronJob "container-report" (imagePullSecrets: my-registry)
        |     - ServiceAccount, ClusterRole, ClusterRoleBinding, ...
        |
        v
  ManagedResource created on seed
        |
        v
  GRM applies bundle to shoot API server (verbatim)


RUNTIME (Shoot)
===============

  Pod scheduled on node
        |
        v
  Kubelet reads imagePullSecrets from Pod spec
        |
        v
  Kubelet authenticates to registry.example.com
  using credentials from Secret "my-registry"
        |
        v
  Image pulled, container starts
```

---

## Security Best Practices

### Never put credentials in the manifest, git, or binary

The manifest file is checked into git and baked into the extension binary at
build time. It must contain only Secret **names** (pointers), never actual
passwords or tokens.

Bad (credentials in manifest):

```yaml
# DO NOT DO THIS
registrySecrets:
  - name: my-registry
    dockerconfigjson: eyJhdXRocyI6ey...  # base64-encoded credentials
```

Good (pointer to seed Secret):

```yaml
registrySecrets:
  - name: my-registry
    seedSecretRef:
      name: my-registry-creds
      namespace: extension-shoot-addon-service
```

### Use short-lived tokens in CI for build-time chart pulling

When CI needs to pull Helm charts from a private OCI registry during
`addon-prepare`, use a short-lived token (CI job token, OIDC token, or
credential helper) instead of a long-lived service account password.

See [build-time-auth.md](build-time-auth.md) for CI-specific examples.

### Rotate seed Secrets regularly

The `docker-registry` Secret on the seed is a long-lived credential. Rotate
it on a schedule (e.g., every 90 days):

```bash
# Generate new password, then update the Secret
kubectl create secret docker-registry my-registry-creds \
  --namespace=extension-shoot-addon-service \
  --docker-server=registry.example.com \
  --docker-username=svc-addon-puller \
  --docker-password='<new-password>' \
  --docker-email=addon-puller@example.com \
  --dry-run=client -o yaml | kubectl apply -f -
```

After updating the seed Secret, the extension will pick up the new credentials
on the next reconciliation loop. You can force immediate reconciliation by
annotating the shoot:

```bash
kubectl annotate shoot <shoot-name> \
  gardener.cloud/operation=reconcile \
  --namespace=garden-<project>
```

### RBAC restricts who can read seed Secrets

The seed Secret is only readable by the extension's ServiceAccount. The default
RBAC configuration grants the extension a Role scoped to its own namespace:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: shoot-addon-service-secret-reader
  namespace: extension-shoot-addon-service
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get"]
    resourceNames: ["my-registry-creds"]
```

This ensures that even other extensions running on the same seed cannot read
the registry credentials.

---

## FAQ

### Do I need imagePullSecrets if I use a cloud provider's native registry with IAM roles?

**No.** If your images are in ECR, GCR, or ACR and your shoot worker nodes have
the appropriate IAM instance profile, Workload Identity, or Managed Identity,
Pattern A handles authentication transparently at the node level. You do not
need `registrySecrets` or `imagePullSecrets` in the manifest.

### What if the shoot namespace doesn't exist yet?

GRM handles this. When GRM applies the ManagedResource bundle to the shoot,
it creates all resources in dependency order. If the bundle includes a
Namespace and a Secret targeting that Namespace, GRM creates the Namespace
first, then the Secret. The addon manifest's `defaultNamespace` field tells
the extension which namespace to target, and GRM ensures it exists before
deploying the Secret and the workloads that reference it.

In practice, the `managed-resources` namespace is created by the extension
as part of the ManagedResource bundle, so the Secret and the DaemonSet/CronJob
all arrive atomically.

### Can multiple addons share one registry Secret?

**Yes.** Declare the registry Secret once in `registrySecrets` and reference
its `name` from multiple addons' `imagePullSecrets` lists. The extension creates
exactly one Secret in the shoot, and all addons reference it. See the
[manifest configuration reference](#multiple-addons-sharing-the-same-registry-secret)
section above for an example.

### What about air-gapped environments?

In air-gapped (disconnected) environments, images are typically pre-loaded onto
nodes or served from a local registry mirror configured at the containerd level.
This is a variant of Pattern A:

1. Configure containerd on each node with a registry mirror pointing to the
   local registry (e.g., `mirror.internal.local`).
2. All image references (e.g., `docker.io/fluent/fluent-bit:3.2.5`) are
   transparently redirected to the mirror.
3. No `imagePullSecrets` are needed because containerd handles auth via its
   host-level config (which can include credentials for the local mirror).

Gardener supports this via the `ContainerRuntime` extension and the
`registryConfig` section of the shoot spec. Consult your platform team for
the specific mirror configuration used in your environment.

### How do I debug image pull failures?

Check these in order:

1. **Pod events**: `kubectl describe pod <pod> -n managed-resources` -- look
   for `ErrImagePull` or `ImagePullBackOff` events with the specific error
   message.

2. **Secret exists in shoot**: `kubectl get secret <name> -n managed-resources`
   -- if missing, the extension did not include it in the bundle.

3. **Secret has correct data**:
   ```bash
   kubectl get secret <name> -n managed-resources \
     -o jsonpath='{.data.\.dockerconfigjson}' | base64 -d | jq .
   ```
   Verify the server URL, username, and auth token are correct.

4. **Pod spec references the Secret**:
   ```bash
   kubectl get daemonset addon-fluent-bit -n managed-resources \
     -o jsonpath='{.spec.template.spec.imagePullSecrets}'
   ```

5. **Seed Secret exists**: On the seed, verify the source Secret is present:
   ```bash
   kubectl get secret my-registry-creds \
     -n extension-shoot-addon-service
   ```

6. **Extension logs**: Check the extension controller logs on the seed for
   errors reading the seed Secret or building the ManagedResource bundle.
