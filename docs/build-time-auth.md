# Build-Time Authentication for Chart Pulling

How to authenticate when pulling Helm charts and images during the
`addon-prepare` build step, before charts are embedded into the extension
binary.

## Table of Contents

- [Overview](#overview)
- [OCI Registries (GHCR, ECR, Private)](#oci-registries-ghcr-ecr-private)
- [Helm Repositories (index.yaml-based)](#helm-repositories-indexyaml-based)
- [Git Repositories](#git-repositories)
- [HTTP/HTTPS URLs](#httphttps-urls)
- [CI/CD Examples](#cicd-examples)
- [Security Guidelines](#security-guidelines)

---

## Overview

The `addon-prepare` tool pulls Helm charts from various sources and embeds them
into the extension binary at build time. Depending on the chart source type,
different authentication mechanisms are required.

| Source Type | Auth Mechanism | Environment / Config |
|---|---|---|
| OCI registry | `helm registry login` | Docker config / credential helper |
| Helm repo | Environment variables | `HELM_REPO_USERNAME` / `HELM_REPO_PASSWORD` |
| Git repo | Token or SSH key | `GH_TOKEN`, `GITLAB_TOKEN`, SSH agent |
| HTTP URL | None (or pre-signed) | No auth required |

**Key principle**: Build-time credentials are used only to pull charts during
the build. They are never embedded in the final binary or deployed to shoots.
Runtime image pull credentials are a separate concern -- see
[registry-credentials.md](registry-credentials.md).

---

## OCI Registries (GHCR, ECR, Private)

When charts are stored as OCI artifacts (e.g., `oci://registry.example.com/charts/fluent-bit`),
authenticate with `helm registry login` before running `addon-prepare`.

### Private OCI Registry

```bash
# Login to a private OCI registry
helm registry login registry.example.com \
  --username "$REGISTRY_USERNAME" \
  --password "$REGISTRY_PASSWORD"

# Now addon-prepare can pull OCI charts
addon-prepare --manifest manifest.yaml --output charts/embedded/addons/
```

### ECR

ECR requires a temporary auth token obtained via the AWS CLI:

```bash
# Get ECR login token (valid for 12 hours)
aws ecr get-login-password --region us-east-1 \
  | helm registry login \
      --username AWS \
      --password-stdin \
      123456789012.dkr.ecr.us-east-1.amazonaws.com

# Pull charts
addon-prepare --manifest manifest.yaml --output charts/embedded/addons/
```

### GitHub Container Registry (GHCR)

```bash
echo "$GH_TOKEN" | helm registry login ghcr.io \
  --username "$GH_USERNAME" \
  --password-stdin
```

### Docker credential helpers

Instead of explicit `helm registry login` calls, you can configure Docker
credential helpers that Helm will use automatically:

```json
// ~/.docker/config.json
{
  "credHelpers": {
    "registry.example.com": "secretservice",
    "123456789012.dkr.ecr.us-east-1.amazonaws.com": "ecr-login"
  }
}
```

With credential helpers configured, `helm registry login` is not needed --
Helm reads credentials from the Docker config automatically.

---

## Helm Repositories (index.yaml-based)

For traditional Helm repositories served over HTTPS with `index.yaml`, use
environment variables to pass credentials:

```bash
# Set credentials via environment
export HELM_REPO_USERNAME="svc-chart-puller"
export HELM_REPO_PASSWORD="<token>"

# Add the repo (Helm uses the env vars for auth)
helm repo add my-charts https://charts.example.com/stable \
  --username "$HELM_REPO_USERNAME" \
  --password "$HELM_REPO_PASSWORD"

helm repo update

# addon-prepare can now resolve charts from this repo
addon-prepare --manifest manifest.yaml --output charts/embedded/addons/
```

If the repository uses a self-signed or internal CA certificate:

```bash
helm repo add my-charts https://charts.internal.example.com/stable \
  --username "$HELM_REPO_USERNAME" \
  --password "$HELM_REPO_PASSWORD" \
  --ca-file /path/to/ca-bundle.crt
```

---

## Git Repositories

When charts are pulled directly from Git repositories, authentication depends
on the hosting platform.

### GitHub (HTTPS with token)

```bash
export GH_TOKEN="ghp_xxxxxxxxxxxx"

# addon-prepare uses the token for HTTPS clones
# URL format: https://${GH_TOKEN}@github.com/org/repo.git
```

Alternatively, configure Git's credential helper:

```bash
git config --global credential.helper store
echo "https://${GH_TOKEN}:x-oauth-basic@github.com" >> ~/.git-credentials
```

### GitLab (HTTPS with CI job token)

```bash
export GITLAB_TOKEN="$CI_JOB_TOKEN"

# URL format: https://gitlab-ci-token:${GITLAB_TOKEN}@gitlab.example.com/org/repo.git
```

### SSH agent

For SSH-based Git access, ensure the SSH agent has the appropriate key loaded:

```bash
eval "$(ssh-agent -s)"
ssh-add ~/.ssh/id_ed25519

# addon-prepare uses SSH for git@ URLs
# URL format: git@github.com:org/repo.git
```

In CI environments, load the SSH key from a CI secret:

```bash
eval "$(ssh-agent -s)"
echo "$SSH_PRIVATE_KEY" | ssh-add -
ssh-keyscan github.com >> ~/.ssh/known_hosts
```

---

## HTTP/HTTPS URLs

When charts are referenced as plain HTTP/HTTPS URLs (e.g., a `.tgz` file
hosted on a web server), no authentication is typically required.

```yaml
# In the addon manifest
chart:
  url: https://artifacts.example.com/charts/fluent-bit-0.48.6.tgz
```

If the server requires authentication, use one of these approaches:

- **Pre-signed URLs**: Generate a time-limited signed URL and use it directly
  in the manifest. This avoids embedding credentials.

- **Pre-download**: Download the chart archive in a CI step before running
  `addon-prepare`, then reference the local file path:

  ```bash
  curl -H "Authorization: Bearer $TOKEN" \
    -o /tmp/fluent-bit-0.48.6.tgz \
    https://artifacts.example.com/charts/fluent-bit-0.48.6.tgz

  # Reference local path in manifest
  ```

- **Netrc file**: For basic auth, configure `~/.netrc`:

  ```
  machine artifacts.example.com
  login svc-chart-puller
  password <token>
  ```

---

## CI/CD Examples

### GitHub Actions

```yaml
name: Build Extension

on:
  push:
    branches: [main]

jobs:
  build:
    runs-on: ubuntu-latest
    permissions:
      contents: read
      packages: read

    steps:
      - uses: actions/checkout@v4

      - name: Login to private OCI registry
        run: |
          helm registry login registry.example.com \
            --username "${{ secrets.REGISTRY_USERNAME }}" \
            --password "${{ secrets.REGISTRY_PASSWORD }}"

      - name: Login to GHCR (for upstream charts)
        run: |
          echo "${{ secrets.GITHUB_TOKEN }}" | helm registry login ghcr.io \
            --username "${{ github.actor }}" \
            --password-stdin

      - name: Prepare addon charts
        run: |
          addon-prepare \
            --manifest manifest.yaml \
            --output charts/embedded/addons/

      - name: Build and push extension image
        run: |
          make release \
            REGISTRY=registry.example.com/your-project
```

### GitLab CI

```yaml
build-extension:
  stage: build
  image: golang:1.22
  variables:
    HELM_EXPERIMENTAL_OCI: "1"
  before_script:
    # Login to private registry using CI variables
    - helm registry login registry.example.com
        --username "$REGISTRY_USERNAME"
        --password "$REGISTRY_PASSWORD"
    # For Git-based chart sources
    - git config --global credential.helper store
    - echo "https://gitlab-ci-token:${CI_JOB_TOKEN}@gitlab.example.com" >> ~/.git-credentials
  script:
    - addon-prepare --manifest manifest.yaml --output charts/embedded/addons/
    - make release REGISTRY=registry.example.com/your-project
```

### AWS CodeBuild

```yaml
version: 0.2

env:
  secrets-manager:
    REGISTRY_USERNAME: "myapp/registry-creds:username"
    REGISTRY_PASSWORD: "myapp/registry-creds:password"

phases:
  pre_build:
    commands:
      # ECR login for base images
      - aws ecr get-login-password --region us-east-1
          | docker login --username AWS --password-stdin
            123456789012.dkr.ecr.us-east-1.amazonaws.com
      # Private registry login for chart pulling
      - helm registry login registry.example.com
          --username "$REGISTRY_USERNAME"
          --password "$REGISTRY_PASSWORD"
  build:
    commands:
      - addon-prepare --manifest manifest.yaml --output charts/embedded/addons/
      - make release REGISTRY=123456789012.dkr.ecr.us-east-1.amazonaws.com/your-project
```

### Generic CI (pre-configured Docker auth)

For CI systems that provide a pre-configured Docker config (e.g., Jenkins
with Docker Pipeline plugin, or any system with a shared `~/.docker/config.json`):

```bash
#!/bin/bash
# CI build script

# Docker config is pre-populated by CI system at ~/.docker/config.json
# Helm reads this automatically for OCI registries.

# For Helm repos, pass credentials via env
export HELM_REPO_USERNAME="${CHART_REPO_USER}"
export HELM_REPO_PASSWORD="${CHART_REPO_PASS}"

# Prepare and build
addon-prepare --manifest manifest.yaml --output charts/embedded/addons/
make release REGISTRY=registry.example.com/your-project
```

---

## Security Guidelines

### Use short-lived tokens

Prefer tokens that expire after the CI job completes:

- **GitHub Actions**: `GITHUB_TOKEN` is scoped to the workflow run and expires
  when the job finishes.
- **GitLab CI**: `CI_JOB_TOKEN` is scoped to the pipeline job.
- **ECR**: `aws ecr get-login-password` returns a token valid for 12 hours.
- **Private registries**: Create service accounts with short expiry periods
  for CI use.

### Use credential helpers over stored passwords

Credential helpers (`docker-credential-ecr-login`, `docker-credential-secretservice`,
etc.) retrieve credentials on demand and never write them to disk. Prefer
these over `helm registry login` which stores credentials in
`~/.config/helm/registry/config.json`.

### Never hardcode credentials

Never put credentials directly in:

- Makefiles
- Dockerfiles
- Shell scripts checked into git
- CI config files (use CI secret variables instead)

Bad:

```bash
# DO NOT DO THIS
helm registry login registry.example.com \
  --username admin \
  --password SuperSecret123
```

Good:

```bash
helm registry login registry.example.com \
  --username "$REGISTRY_USERNAME" \
  --password "$REGISTRY_PASSWORD"
```

### Separate build-time and runtime credentials

Build-time credentials (for pulling charts during `addon-prepare`) and runtime
credentials (for pulling images in shoots) serve different purposes and should
use different service accounts:

| Purpose | Account | Permissions |
|---|---|---|
| Build-time chart pull | `svc-chart-puller` | Read-only on chart repositories |
| Build-time image push | `svc-image-pusher` | Push to extension image repository |
| Runtime image pull | `svc-addon-puller` | Pull from addon image repository |

This limits blast radius -- if the build-time chart-pull credential is
compromised, the attacker cannot push images or access runtime secrets.

### Audit credential usage

Enable audit logging on your registry to track which service accounts are
pulling charts and when. This helps detect compromised credentials and ensures
compliance with credential rotation policies.
