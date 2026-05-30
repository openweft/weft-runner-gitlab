#!/bin/bash
# runner-init — PID-1-adjacent entrypoint for the weft-runner-gitlab microVM.
#
# The host daemon (weft-runner-gitlab) writes the per-job JobSpec to
# /run/weft/cfg/gitlab-job.json via the cfg share. weft-init mounts that
# share very early, but on slower hypervisors the mount can lag a few
# hundred ms behind our exec, so we busy-wait briefly before declaring it
# missing.

set -euo pipefail

log() { printf 'runner-init: %s\n' "$*" >&2; }

JOB_FILE=/run/weft/cfg/gitlab-job.json
SHUTDOWN_FIFO=/run/weft-shutdown

log "waiting for ${JOB_FILE}"
deadline=$(( $(date +%s) + 30 ))
while [ ! -s "${JOB_FILE}" ]; do
    if [ "$(date +%s)" -ge "${deadline}" ]; then
        log "timeout: ${JOB_FILE} never appeared after 30s; cfg share not mounted?"
        exit 1
    fi
    sleep 0.2
done
log "found JobSpec ($(wc -c <"${JOB_FILE}") bytes)"

# Extracting the actual gitlab-ci.yml step body from a JobSpec needs the
# upstream gitlab-runner internals (it builds a synthetic shell script
# from the .steps[] tree). For this milestone we only wire the lifecycle:
# we honour an opt-in top-level `.Script` array if the daemon (or a test)
# put one there, otherwise we exit 0 so the VM teardown path is exercised.
SCRIPT_LINES=$(jq -r '.Script // empty | if type == "array" then join("\n") else . end' "${JOB_FILE}")

rc=0
if [ -n "${SCRIPT_LINES}" ]; then
    log "executing JobSpec.Script (length $(printf '%s' "${SCRIPT_LINES}" | wc -c) bytes)"
    set +e
    runuser -u runner -- bash -lc "${SCRIPT_LINES}"
    rc=$?
    set -e
    log "script exited rc=${rc}"
else
    log "no script in JobSpec, this milestone only ships the lifecycle skeleton"
fi

if [ -e "${SHUTDOWN_FIFO}" ]; then
    log "signalling weft-init via ${SHUTDOWN_FIFO}"
    printf 'runner-exit %d\n' "${rc}" >"${SHUTDOWN_FIFO}" || true
fi

exit "${rc}"
