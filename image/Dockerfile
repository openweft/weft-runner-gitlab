# syntax=docker/dockerfile:1.7

# In-VM runtime image for weft-runner-gitlab.
#
# Boot contract (see ../runner/job.go):
#   - weft-init mounts the cfg share at /run/weft/cfg/
#   - gitlab-job.json is written there by the host daemon, one file per VM
#   - ENTRYPOINT busy-waits for that file, parses it, then runs the job script

FROM debian:bookworm-slim AS runtime
LABEL org.opencontainers.image.source="https://github.com/openweft/weft-runner-gitlab"
LABEL org.opencontainers.image.description="In-VM rootfs for weft-runner-gitlab (gitlab-runner)"
LABEL org.opencontainers.image.licenses="Apache-2.0"

ARG TARGETARCH

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates=\* \
        curl=\* \
        tar=\* \
        git=\* \
        jq=\* \
        bash=\* \
    && rm -rf /var/lib/apt/lists/*

# gitlab-runner publishes per-arch single-binary releases at the
# `/latest/binaries/` path; TARGETARCH already matches their naming
# (amd64|arm64) so no remap is needed.
RUN set -eux; \
    case "${TARGETARCH}" in \
        amd64|arm64) : ;; \
        *) echo "unsupported arch: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL -o /usr/local/bin/gitlab-runner \
        "https://gitlab-runner-downloads.s3.amazonaws.com/latest/binaries/gitlab-runner-linux-${TARGETARCH}"; \
    chmod +x /usr/local/bin/gitlab-runner

# uid 1000: matches the conventional non-root user inside the microVM.
RUN useradd --create-home --uid 1000 --shell /bin/bash runner

COPY runner-init.sh /usr/local/bin/runner-init
RUN chmod +x /usr/local/bin/runner-init

ENTRYPOINT ["/usr/local/bin/runner-init"]
