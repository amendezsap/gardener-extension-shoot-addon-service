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

**Non-Job, non-Secret resources** (ServiceAccounts, RBAC) are included in the ManagedResource as regular Kubernetes resources. The GRM applies them alongside main chart resources. These are idempotent and safe to re-apply.

**Hook Secrets** are included in the MR with two GRM annotations:
- `resources.gardener.cloud/ignore: "true"` — GRM creates on first deploy, never updates (preserves Job-populated data)
- `resources.gardener.cloud/keep-object: "true"` — Secret survives MR deletion so delete hook Jobs can still mount it (e.g., `wiz-api-token` needed for connector deregistration)

The extension cleans up kept Secrets after delete hooks complete. Hook Secret names are persisted alongside delete hooks in the `addon-delete-hooks-*` Secret for reliable cleanup. On seed renders, Secrets are also applied directly before Jobs to ensure proper ordering.

**Hook Jobs** are handled differently depending on context:

- **Seed renders** (runtime cluster): Jobs are applied directly by the actuator with deduplication and completion wait. Hook Secrets are applied directly first (create-or-skip) to ensure they exist before Jobs run. The actuator checks if the Job already exists by comparing a spec hash annotation. Same hash = skip. Different hash (chart upgrade) = delete old + create new. Newly created Jobs are polled for completion (120s timeout).

- **Shoot renders** (shoot clusters via temp MR): Jobs are NOT included in the persistent addon MR. Instead, each Job is applied through a temporary shoot-class MR with spec hash deduplication (tracked in `AddonStatus.HookJobsCompleted`). The GRM applies the Job to the shoot, the extension polls for completion, then deletes the temp MR. On subsequent reconciles, Jobs with unchanged spec hashes are skipped entirely — no MR, no GRM interaction. On chart upgrades (different hash), the Job runs again via a fresh temp MR. This mirrors Helm's hook lifecycle: run once per install/upgrade event, then gone.

### Delete Hooks (pre-delete, post-delete)

Delete hooks are **persisted in a Kubernetes Secret** (`addon-delete-hooks-<addonName>`) in the control-plane namespace. This survives pod restarts and is available even after the addon is removed from the manifest.

Delete hook execution differs by render context:

**Shoot renders**: Delete hooks are applied via a **temporary shoot-class MR**. The GRM applies the hook resources (SA, Role, RoleBinding, Job) to the shoot cluster — where the addon's Secrets exist. The extension polls MR status (`ResourcesHealthy`) for Job completion and deletes the temporary MR afterwards. This is necessary because the extension cannot access shoot clusters directly.

**Seed renders**: Delete hooks are applied directly to the runtime cluster using an uncached client (same as install hooks).

Delete hooks execute in two scenarios:

**1. Extension deletion** (shoot removed):
- Pre-delete hooks run before MR deletion (via temporary shoot MR)
- MR deleted
- Post-delete hooks run after MR deletion (via temporary shoot MR)

**2. Addon removal** (addon removed from manifest):
- The extension detects removed addons by diffing current manifest against previous ProviderStatus
- Pre-delete hooks run (shoot: temporary MR, seed: direct application)
- Addon MR deleted
- Post-delete hooks run
- Persisted delete hooks Secret cleaned up

If a delete hook Job fails or times out, behavior depends on `deleteFailurePolicy`:
- `Continue` (default): logs the failure and proceeds with deletion
- `Abort`: returns an error, blocking addon removal

### Test Hooks

Excluded by default. Test hooks are for `helm test` and not applicable to automated deployment.

## Addon Removal Detection

When an addon is removed from the manifest, the extension automatically:

**For shoot addons:**
- Compares current manifest against `ProviderStatus.Addons` (tracks deployed addons)
- Executes pre-delete hooks on the shoot via temporary MR
- Deletes the shoot ManagedResource
- Executes post-delete hooks on the shoot via temporary MR
- Cleans up persisted delete hooks Secret

**For seed addons:**
- Compares current manifest against `seed-addon-state` ConfigMap in the extension namespace
- Executes pre-delete hooks directly on the runtime
- Deletes seed ManagedResources for removed addons
- Executes post-delete hooks directly on the runtime
- Cleans up persisted delete hooks Secret
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

For seed renders, the extension applies hook Jobs directly and waits for them to complete (120s timeout) before returning. This ensures hook Jobs (e.g., connector registration that creates a Secret) finish before the GRM applies Deployments that mount those Secrets. On subsequent reconciles, existing Jobs with the same spec hash are skipped — no wait needed.

## Limitations

| Limitation | Description |
|---|---|
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
