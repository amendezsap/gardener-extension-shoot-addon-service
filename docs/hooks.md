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

Included in the ManagedResource as regular Kubernetes resources. The GRM applies them alongside main chart resources. Dependencies resolve eventually:

1. Secrets, ServiceAccounts, RBAC → exist immediately after apply
2. Jobs → run and create additional resources (e.g., connector secrets)
3. Deployments → retry pod mount until dependent secrets exist

**Hook weight ordering:** Hooks with lower weights (e.g., `-1`) are ordered before higher weights in the MR. However, the GRM applies all resources simultaneously. Weight ordering is best-effort — the eventual consistency model resolves dependencies regardless of apply order.

### Delete Hooks (pre-delete, post-delete)

Stored separately and executed by the extension's `Delete()` function:

1. **Pre-delete hooks** run before MR deletion. The extension applies hook resources (Jobs, SAs, RBAC) directly and waits for Job completion.
2. MR is deleted (GRM tears down main resources).
3. **Post-delete hooks** run after MR deletion.

If a delete hook Job fails or times out, behavior depends on `deleteFailurePolicy`:
- `Continue` (default): logs the failure and proceeds with deletion
- `Abort`: returns an error, blocking addon removal until the hook succeeds or is manually resolved

### Test Hooks

Excluded by default. Test hooks are for `helm test` and not applicable to automated deployment.

## Hook Annotations

When `stripAnnotations` is true (default), the following annotations are removed from included hook resources:
- `helm.sh/hook`
- `helm.sh/hook-weight`
- `helm.sh/hook-delete-policy`

For Job resources, `resources.gardener.cloud/delete-on-invalid-update: "true"` is added. This tells the GRM to delete and recreate Jobs when their immutable `spec.template` changes between reconciles — replacing the Helm `hook-delete-policy: before-hook-creation` behavior.

## Limitations

| Limitation | Description |
|---|---|
| **No ordered execution** | The GRM applies all MR resources simultaneously. Pre-install hooks and main resources deploy at the same time. Dependencies resolve via Kubernetes retry mechanics (~10-30s). |
| **Completed Jobs persist** | Helm's `hook-delete-policy: hook-succeeded` is not enforced. Completed Jobs remain until TTL or manual cleanup. Recommend `ttlSecondsAfterFinished` in chart values. |
| **Pre-delete hooks require stored state** | Delete hooks are stored in memory during Reconcile. If the extension pod restarts between Reconcile and Delete, delete hooks may not be available. |
| **Rollback hooks not supported** | `pre-rollback` and `post-rollback` are not applicable — the MR model has no rollback concept. |
| **Hook weights are best-effort** | Resources are ordered by weight in the MR secret but applied simultaneously by the GRM. |

## Supported Hook Types

| Hook Type | Supported | Handling |
|---|---|---|
| `pre-install` | Yes | Included in MR |
| `post-install` | Yes | Included in MR |
| `pre-upgrade` | Yes | Included in MR |
| `post-upgrade` | Yes | Included in MR |
| `pre-delete` | Yes | Executed by extension Delete() |
| `post-delete` | Yes | Executed by extension Delete() |
| `pre-rollback` | No | Not applicable |
| `post-rollback` | No | Not applicable |
| `test` | No | Excluded by default |
