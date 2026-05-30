# weft-runner-gitlab / in-VM runtime image

This directory builds the OCI image booted as a microVM by `weft-runner-gitlab`.
Each job runs in a fresh VM with this rootfs.

## Boot contract

`weft-runner-gitlab` (the host daemon) long-polls GitLab for a job assignment
and exposes the resulting JobSpec to the VM through the cfg share:

| Path inside VM                       | Producer                | Consumer                        |
| ------------------------------------ | ----------------------- | ------------------------------- |
| `/run/weft/cfg/gitlab-job.json`      | host daemon, before boot| `runner-init` (this image)      |
| `/run/weft-shutdown` (if present)    | weft-init               | `runner-init`, post-exit signal |

`runner-init` busy-waits up to 30 s for `gitlab-job.json`, parses it with
`jq`, then executes the `.Script` array (joined on newlines) as bash via
`runuser -u runner`. If `.Script` is empty the entrypoint logs a notice
and exits 0 — that exercises the VM lifecycle without needing a real
step-script extraction path, which still lives inside upstream
`gitlab-runner`'s job-builder.

The `gitlab-runner` binary itself is baked in at `/usr/local/bin/gitlab-runner`
for the day we want to drive it directly instead of synthesising scripts.

weft-init treats the ENTRYPOINT exit as a VM stop.

## Build + push

```
docker buildx build \
    --platform linux/amd64,linux/arm64 \
    -t ghcr.io/openweft/weft-runner-gitlab:v0.1.0 \
    --push \
    image/
```

CI (`.github/workflows/image.yml`) builds on manual dispatch and on `v*`
tags only — dev pushes to `main` deliberately do not publish.

## Use with `weft-runner-gitlab`

```
weft-runner-gitlab register \
    --url=https://gitlab.com \
    --registration-token=<gl-reg-token> \
    --config=/etc/weft-runner-gitlab.json
weft-runner-gitlab run \
    --config=/etc/weft-runner-gitlab.json \
    --weft-endpoint=unix:///var/run/weft/agent.sock \
    --image=ghcr.io/openweft/weft-runner-gitlab:v0.1.0 \
    --concurrency=4
```

The daemon never reaches into this image; the only coupling is the cfg-share
filename above. Anything else — extra tooling, additional runner tags, a
non-Debian base — is a private decision of this Dockerfile.

## gitlab-runner version

We track the upstream `latest` channel at build time. Pin by replacing
the URL in the Dockerfile with a versioned release (e.g.
`/v17.0.0/binaries/gitlab-runner-linux-${TARGETARCH}`) when reproducible
builds matter.
