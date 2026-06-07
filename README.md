# weft-runner-gitlab

Self-hosted GitLab CI runner backed by **weft** ephemeral microVMs.

## What it does

`weft-runner-gitlab` registers as a GitLab self-hosted runner against
an instance / group / project, then for each job assigned to it:

1. Asks a weft cluster to spawn a fresh **ephemeral microVM** from an OCI rootfs
   (e.g. `registry.gitlab.com/gitlab-org/gitlab-runner/ubuntu:24.04`) — clean
   state per job, isolated, throwaway.
2. Runs `gitlab-runner exec` inside that VM via a thin agent (boot-time
   bootstrap drops the runner binary + token + workdir mount).
3. Streams logs back via `/api/v4/jobs/{id}/trace`, marks the job done, and
   tears the VM down.

Sibling of `weft-runner-github` and `weft-runner-forgejo` — the three share
the microVM-spawn primitive but plug into their respective CI control planes.
Each implements the platform's own protocol (GitLab's `/api/v4/runners`
+ `/api/v4/jobs/request` here, GitHub Actions's Runtime API in the GitHub
sibling, Forgejo's Connect-over-JSON in the Forgejo one).

## Status

**Operational** — the seven implementation steps in `doc.go` ship :
`POST /api/v4/runners` registration, runner token + ID persisted via
`PersistedConfig`, long-poll loop through `POST /api/v4/jobs/request`,
microVM dispatch via weft-client, log shipping by the in-VM
`gitlab-runner` directly to `/api/v4/jobs/{id}/trace`. See `doc.go` for
the per-step status.

## Quick start

```sh
# 1. Get a runner registration token from GitLab. Two options :
#    - Group-level : Group → Settings → CI/CD → Runners → Register
#    - Instance-level (admin only) : Admin → CI/CD → Runners
#    The token IS the bootstrap credential — no PAT or app indirection.

# 2. Register the runner.
weft-runner-gitlab register \
  --url https://gitlab.com \
  --registration-token $GITLAB_RUNNER_TOKEN \
  --description "weft microVM arm64 runner" \
  --labels "weft,microvm,arm64"

# 3. Start polling for jobs. Each job spawns a fresh microVM on the
#    target weft cluster.
weft-runner-gitlab run \
  --weft-endpoint tcp:weft.example.com:7330 \
  --image registry.gitlab.com/gitlab-org/gitlab-runner/ubuntu:24.04
```

## Architecture

See `doc.go` for the design intent and component boundaries ;
`runner/runner.go` for the lifecycle layer, `runner/gitlab.go` for the
GitLab v4 REST client.
