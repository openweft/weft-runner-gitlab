#!/bin/bash
# runner-init — PID-1-adjacent entrypoint for the weft-runner-gitlab microVM.
#
# The host daemon (weft-runner-gitlab) writes two files to /run/weft/cfg
# via the cfg share:
#   - gitlab-job.json    — the full JobSpec (used for metadata + CI_* env)
#   - gitlab-script.sh   — the rendered bash script (pre-built by the
#                          daemon's renderJobScript; we exec it verbatim).
#
# weft-init mounts the cfg share very early, but on slower hypervisors the
# mount can lag a few hundred ms behind our exec, so we busy-wait briefly
# before declaring it missing.

set -euo pipefail

log() { printf 'runner-init: %s\n' "$*" >&2; }

JOB_FILE=/run/weft/cfg/gitlab-job.json
SCRIPT_FILE=/run/weft/cfg/gitlab-script.sh
SHUTDOWN_FIFO=/run/weft-shutdown

log "waiting for ${JOB_FILE}"
deadline=$(( $(date +%s) + 30 ))
while [ ! -s "${JOB_FILE}" ] || [ ! -s "${SCRIPT_FILE}" ]; do
    if [ "$(date +%s)" -ge "${deadline}" ]; then
        log "timeout: ${JOB_FILE}/${SCRIPT_FILE} never appeared after 30s; cfg share not mounted?"
        exit 1
    fi
    sleep 0.2
done
log "found JobSpec ($(wc -c <"${JOB_FILE}") bytes) and script ($(wc -c <"${SCRIPT_FILE}") bytes)"

# Export GitLab Runner protocol env vars derived from JobSpec metadata.
# The full upstream gitlab-runner exports ~80 CI_* keys; we ship the
# minimum the script body and `.variables[]` consumers expect to see.
# Anything else .variables[] declares is already exported by the script
# itself (renderJobScript emits one `export K=V` per variable).
CI_JOB_ID=$(jq -r '.id // empty' "${JOB_FILE}")
CI_JOB_TOKEN=$(jq -r '.token // empty' "${JOB_FILE}")
CI_PROJECT_URL=$(jq -r '(.variables[] | select(.key=="CI_PROJECT_URL") | .value) // empty' "${JOB_FILE}")
CI_PROJECT_DIR=$(jq -r '(.variables[] | select(.key=="CI_PROJECT_DIR") | .value) // "/builds/project"' "${JOB_FILE}")
export CI_JOB_ID CI_JOB_TOKEN CI_PROJECT_URL CI_PROJECT_DIR
log "CI_JOB_ID=${CI_JOB_ID} CI_PROJECT_DIR=${CI_PROJECT_DIR}"

# stdout/stderr are already wired to the VM's console which weft-init
# forwards on; `weft microvm logs --follow` reads from there, so the
# daemon's PATCH /trace path sees every byte we emit here.
rc=0
set +e
runuser -u runner -- bash "${SCRIPT_FILE}"
rc=$?
set -e
log "script exited rc=${rc}"

if [ -e "${SHUTDOWN_FIFO}" ]; then
    log "signalling weft-init via ${SHUTDOWN_FIFO}"
    printf 'runner-exit %d\n' "${rc}" >"${SHUTDOWN_FIFO}" || true
fi

exit "${rc}"
