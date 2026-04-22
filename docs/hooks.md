# Helm Hook Support

## Why This Exists

Gardener's built-in chart renderer ([`releaseutil.SortManifests`](https://pkg.go.dev/helm.sh/helm/v3/pkg/releaseutil)) separates Helm hooks from regular manifests and **discards hooks entirely**. This is by design — the ManagedResource/GRM model has no concept of hook lifecycle. Gardener's own components don't use Helm hooks; they use Go-based component deployment.

This extension provides a **hook-aware renderer** that re-integrates hooks with proper lifecycle management. It enables third-party charts with lifecycle hooks (connector registration, API token creation, cleanup Jobs) to deploy correctly through ManagedResources.

## How It Differs From Helm

| Behavior | Helm | This Extension |
|---|---|---|
| Hook execution timing | Synchronous, ordered by weight, before/after chart resources | Async for shoots (temp MR), sync for seeds (direct apply) |
| `hook-delete-policy: hook-succeeded` | Helm deletes the resource after success | Not enforced — completed Jobs persist until chart TTL or manual cleanup |
| `hook-delete-policy: before-hook-creation` | Helm deletes old hook before creating new | Honored via spec hash comparison + delete/recreate on seeds. On shoots, old Job is cleaned up by temp MR deletion |
| `ttlSecondsAfterFinished` | Kubernetes honors it normally | Honored — state-based dedup prevents recreation after TTL cleanup |
| Hook ordering by weight | Hooks run in weight order | Not enforced — all hooks are applied simultaneously. Use Kubernetes dependencies (volume mounts, init containers) for ordering |
| `pre-rollback` / `post-rollback` | Runs on `helm rollback` | Not supported — the MR model has no rollback concept |
| `test` hooks | Runs on `helm test` | Excluded by default |

## Architecture

### Seed Renders (Runtime Cluster)

The extension has **direct access** to the runtime cluster. Hook resources are applied directly using an uncached Kubernetes client:

1. **Hook Secrets** applied first (create-or-skip) — ensures they exist before Jobs run
2. **Hook Jobs** applied with two-tier deduplication:
   - **Tier 1 (state)**: spec hashes tracked in `seed-addon-state` ConfigMap. If hash matches, skip without checking the cluster. Handles TTL-deleted Jobs.
   - **Tier 2 (cluster)**: fallback for first deploy or state loss. Checks cluster for existing Job with matching `shoot-addon-service/spec-hash` annotation.
3. Newly created Jobs are **polled for completion** (120s timeout) — blocks the reconcile to ensure ordering (Job output must exist before GRM applies dependent Deployments)
4. **Non-Job hook resources** (SAs, Roles, RoleBindings) go into the seed MR as regular resources (idempotent)

### Shoot Renders (Shoot Clusters via MR)

The extension **cannot access shoot clusters directly**. All resources are applied via ManagedResources processed by the GRM:

1. **Hook Secrets** in the persistent addon MR with `resources.gardener.cloud/ignore: "true"` — GRM creates once, never overwrites Job-populated data
2. **Hook Jobs** are NOT in the persistent addon MR. Each Job is applied via a **temporary shoot-class MR** with spec hash deduplication:
   - First deploy: temp MR created (non-blocking), GRM applies Job to shoot, next reconcile checks MR status, records hash, deletes temp MR
   - Steady state: hash matches → skip entirely (<1ms, no MR, no GRM interaction)
   - Chart upgrade: hash differs → new temp MR created
3. **Non-Job hook resources** (SAs, Roles, RoleBindings) go into the persistent MR (idempotent)

### Delete Hooks

Delete hook templates are **persisted in a Kubernetes Secret** (`addon-delete-hooks-<addonName>`) during install. This survives pod restarts and is available after the addon is removed from the manifest.

**Shoot path**: delete hooks are applied via temporary shoot-class MR — the GRM applies them to the shoot where the addon's Secrets exist. The extension polls MR status for completion.

**Seed path**: delete hooks are applied directly to the runtime cluster.

For `target: global` addons (deploy to both seed and shoot), delete hooks run only from the shoot path to avoid double execution.

## Enabling Hooks

```yaml
addons:
  - name: my-addon
    chart:
      oci: oci://registry/my-chart
      version: "1.0.0"
    hooks:
      include: true
```

When `hooks` is absent or `include` is false, hooks are silently dropped (historical Gardener behavior).

## Configuration

| Field | Default | Description |
|---|---|---|
| `include` | `false` | Enable hook rendering |
| `stripAnnotations` | `true` | Remove `helm.sh/hook*` annotations from rendered resources |
| `deleteTimeout` | `300` | Seconds to wait for pre/post-delete Jobs |
| `deleteFailurePolicy` | `Continue` | `Continue` proceeds on hook failure. `Abort` blocks deletion. |
| `excludeTypes` | `["test"]` | Hook types to exclude |

## Supported Hook Types

| Hook Type | Supported | Handling |
|---|---|---|
| `pre-install` | Yes | Non-Job: in MR. Job: direct apply (seed) or temp MR (shoot) |
| `post-install` | Yes | Same as pre-install |
| `pre-upgrade` | Yes | Same as pre-install |
| `post-upgrade` | Yes | Same as pre-install |
| `pre-delete` | Yes | Persisted in Secret, executed on addon removal or Extension deletion |
| `post-delete` | Yes | Same as pre-delete |
| `pre-rollback` | No | Not applicable — MR model has no rollback |
| `post-rollback` | No | Not applicable |
| `test` | No | Excluded by default |

## Limitations

| Limitation | Description | Workaround |
|---|---|---|
| **`hook-succeeded` not enforced** | Completed Jobs persist until chart TTL or manual cleanup. Helm deletes them immediately on success. | Set `ttlSecondsAfterFinished` in your chart's Job spec. The extension's state-based dedup prevents recreation after TTL cleanup. |
| **Hook weight ordering not enforced** | Hooks are applied simultaneously, not in weight order. | Use Kubernetes-native ordering: volume mounts (Job waits for Secret), init containers, or readiness probes. |
| **Rollback hooks not supported** | `pre-rollback` and `post-rollback` are not applicable. | The MR model has no rollback concept. Revert by changing the chart version in the manifest. |
| **Shoot-side Jobs are async** | On shoots, hook Jobs run asynchronously via GRM. The extension doesn't block waiting for completion (to avoid queue jamming). Completion is detected on the next reconcile. | If your Job must complete before other resources start, use Kubernetes dependencies (volume mounts on Job-created Secrets). |
| **First deploy blocks on seeds** | On seed renders, the first hook Job execution blocks the reconcile (120s timeout). Subsequent reconciles skip instantly. | Expected behavior — needed for ordering. Jobs typically complete in <10s. |
| **Manual Job deletion causes recreation on seeds** | If a completed hook Job is manually deleted on the seed, the cluster-based fallback (tier 2) won't find it and will recreate it. | Don't manually delete completed hook Jobs on seeds. State-based dedup (tier 1) prevents recreation after TTL deletion. |

## GRM Annotations Used

These annotations are set on individual resources within the ManagedResource Secret data:

| Annotation | Applied to | Purpose |
|---|---|---|
| `resources.gardener.cloud/ignore` | Hook Secrets | GRM creates once, never overwrites Job-populated data |

Hook Jobs are **not in the persistent MR** (applied via temp MR or direct), so no GRM annotations are needed on them.

## RBAC Requirements

Chart version `0.1.5`+ includes RBAC for direct hook resource application on seeds:
- `batch/jobs` — create, get, list, watch, delete
- `serviceaccounts` — get, create, update, delete
- `rbac.authorization.k8s.io/roles`, `rolebindings`, `clusterroles`, `clusterrolebindings` — get, create, update, delete

## State Tracking

| State | Location | Purpose |
|---|---|---|
| `AddonStatus.HookJobsCompleted` | Extension CR `status.providerStatus` | Shoot-side hook Job spec hashes (per-shoot) |
| `hookJobHashes` key | `seed-addon-state` ConfigMap | Seed-side hook Job spec hashes (per-extension) |
| `addon-delete-hooks-*` Secret | Shoot control-plane namespace or extension namespace | Persisted delete hook templates + hook Secret names |
| `addonMappings` key | `seed-addon-state` ConfigMap | MR name → addon name mapping for seed addon removal |

## Contributing

When adding hook support to a new chart:

1. Set `hooks.include: true` in the addon manifest
2. Ensure hook Jobs are **idempotent** — they may run more than once (on chart upgrade, pod restart, or state loss)
3. Use `ttlSecondsAfterFinished` for cleanup — the extension honors it
4. Don't rely on hook weight ordering — use Kubernetes-native dependencies instead
5. Test both install and removal lifecycle (delete hooks are critical for cleanup)
6. For delete hooks: ensure the Job's dependencies (API tokens, credentials) are available as hook Secrets, not regular chart Secrets — hook Secrets get `ignore` annotation and won't be overwritten by GRM
