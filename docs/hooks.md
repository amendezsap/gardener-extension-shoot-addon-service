# Helm Hook Support

The extension supports rendering Helm hook-annotated templates that are normally skipped by Gardener's chart renderer. This enables charts with lifecycle hooks (connector registration, API token creation, cleanup Jobs) to deploy correctly.

## Enabling Hooks

Add `hooks.include: true` to the addon configuration in the manifest:

```yaml
addons:
  - name: my-addon
    chart:
      oci: oci://registry/my-chart
      version: "1.0.0"
    hooks:
      include: true
```

When `hooks` is absent or `include` is false, the historical behavior is preserved — hooks are silently dropped.

## Configuration

| Field | Default | Description |
|---|---|---|
| `include` | `false` | Enable hook rendering |
| `stripAnnotations` | `true` | Remove `helm.sh/hook*` annotations from rendered resources |
| `deleteTimeout` | `300` | Seconds to wait for pre/post-delete Jobs |
| `deleteFailurePolicy` | `Continue` | `Continue` proceeds with deletion on hook failure. `Abort` blocks deletion. |
| `excludeTypes` | `["test"]` | Hook types to exclude |

### Full Example

```yaml
addons:
  - name: security-connector
    chart:
      oci: oci://registry/security-integration
      version: "4.0.0"
    hooks:
      include: true
      stripAnnotations: true
      deleteTimeout: 120
      deleteFailurePolicy: Continue
      excludeTypes:
        - test
```

## How Hooks Are Handled

### Install/Upgrade Hooks (pre-install, post-install, pre-upgrade, post-upgrade)

**Non-Job resources** (Secrets, ServiceAccounts, RBAC) are included in the ManagedResource as regular Kubernetes resources. The GRM applies them alongside main chart resources.

**Job resources** are handled differently depending on context:

- **Seed renders** (runtime cluster): Jobs are applied directly by the actuator with deduplication. The actuator checks if the Job already exists by comparing a spec hash annotation. Same hash = skip. Different hash (chart upgrade) = delete old + create new. This prevents the GRM from recreating completed Jobs every 60s.

- **Shoot renders** (shoot clusters): Jobs are included in the MR so the GRM applies them to the shoot cluster. The extension cannot access shoot clusters directly.

### Delete Hooks (pre-delete, post-delete)

Delete hooks are **persisted in a Kubernetes Secret** (`addon-delete-hooks-<addonName>`) in the shoot's control-plane namespace. This survives pod restarts and is available even after the addon is removed from the manifest.

Delete hooks execute in two scenarios:

**1. Extension deletion** (shoot removed):
- Pre-delete hooks run before MR deletion
- Post-delete hooks run after MR deletion

**2. Addon removal** (addon removed from manifest):
- The extension detects removed addons by diffing current manifest against previous ProviderStatus
- Pre-delete hooks are read from the persisted Secret and applied
- Shoot and seed MRs are deleted
- Post-delete hooks run
- The hook Secret is cleaned up

If a delete hook Job fails or times out, behavior depends on `deleteFailurePolicy`:
- `Continue` (default): logs the failure and proceeds with deletion
- `Abort`: returns an error, blocking addon removal

### Test Hooks

Excluded by default. Test hooks are for `helm test` and not applicable to automated deployment.

## Addon Removal Detection

When an addon is removed from the manifest, the extension automatically:

**For shoot addons:**
- Compares current manifest against `ProviderStatus.Addons` (tracks deployed addons)
- Executes pre-delete hooks from persisted Secret
- Deletes the shoot ManagedResource
- Executes post-delete hooks
- Cleans up hook and state Secrets

**For seed addons:**
- Compares current manifest against `seed-addon-state` ConfigMap in the extension namespace
- Deletes seed ManagedResources for removed addons
- Updates the state ConfigMap

## Hook Annotations

When `stripAnnotations` is true (default), the following annotations are removed:
- `helm.sh/hook`
- `helm.sh/hook-weight`
- `helm.sh/hook-delete-policy`

## RBAC Requirements

Chart version `0.1.5`+ includes RBAC for direct hook resource application:
- `batch/jobs` — create, get, list, watch, delete
- `serviceaccounts` — get, create, update, delete
- `rbac.authorization.k8s.io/roles`, `rolebindings`, `clusterroles`, `clusterrolebindings` — get, create, update, delete

These permissions are needed for seed-render direct Job application and delete hook lifecycle management.

## Ordering and Dependencies

The GRM applies MR resources in Kubernetes kind order (Helm's InstallOrder):
- Namespaces → Secrets → ServiceAccounts → Roles → Jobs → Deployments

For seed renders, the extension applies hook Jobs directly after the MR is created. On first deploy, there may be a ~30s delay while:
1. GRM applies MR (Secrets, SAs, RBAC exist immediately)
2. Hook Job runs (creates additional Secrets)
3. Deployment retries pod mount until the Job-created Secret exists

Subsequent reconciles skip the Job (exists with same spec hash) and the Deployment starts immediately.

## Limitations

| Limitation | Description |
|---|---|
| **Shoot-side Job churn** | On shoot renders, hook Jobs are in the MR and may be recreated by the GRM each reconcile cycle. Use `ttlSecondsAfterFinished` in chart values to limit accumulation. |
| **No strict pre-install ordering** | Jobs and Deployments deploy simultaneously via the MR. Dependencies resolve via Kubernetes retry (~30s on first deploy). |
| **Completed Jobs persist** | Helm's `hook-delete-policy: hook-succeeded` is not enforced. Completed Jobs remain until TTL or manual cleanup. |
| **Rollback hooks not supported** | `pre-rollback` and `post-rollback` are not applicable — the MR model has no rollback concept. |

## Supported Hook Types

| Hook Type | Supported | Handling |
|---|---|---|
| `pre-install` | Yes | Non-Job: in MR. Job: direct apply (seed) or in MR (shoot) |
| `post-install` | Yes | Same as pre-install |
| `pre-upgrade` | Yes | Same as pre-install |
| `post-upgrade` | Yes | Same as pre-install |
| `pre-delete` | Yes | Persisted in Secret, executed on addon removal or Extension deletion |
| `post-delete` | Yes | Same as pre-delete |
| `pre-rollback` | No | Not applicable |
| `post-rollback` | No | Not applicable |
| `test` | No | Excluded by default |
