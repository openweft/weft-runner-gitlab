// Package main hosts the weft-runner-gitlab binary — a self-hosted GitLab
// Actions runner that executes each incoming job in a fresh weft microVM.
//
// # Why
//
// The default GitLab-hosted runners share resources and OS images across
// every customer, and the "runs-on: self-hosted" alternative usually means
// either persistent bare metal (slow to reset, leaks state across jobs) or a
// docker-in-docker shim (no real isolation from the host). weft-runner-gitlab
// gives each job its own VM-isolated environment by riding on the same
// microVM spawn primitive as the rest of weft (`weft microvm run`, OCI rootfs
// → boot under Apple-VZ or QEMU/KVM).
//
// # Components
//
//	[GitLab Service] ⇄ runner/gitlab.go ⇄ runner/runner.go ⇄ runner/job.go ⇄ [weft cluster]
//	         /api/v4 + long-poll     lifecycle         per-job            gRPC
//
//   - runner/gitlab.go: registers the runner against an instance / group /
//     project via POST /api/v4/runners with a runner registration token ;
//     long-polls POST /api/v4/jobs/request for assigned jobs ; reports
//     completion via PUT /api/v4/jobs/{id} and ships logs via PATCH
//     /api/v4/jobs/{id}/trace.
//   - runner/runner.go: the daemon loop — owns the connection to GitLab, the
//     connection to weft, and the per-job state machine.
//   - runner/job.go: turns one job spec into a microVM lifecycle —
//     RegisterMicroVM → StartVM → stream output → DeleteVM — with a cancel
//     path tied to GitLab's job cancellation flag.
//
// # Sibling runners
//
// All three runners (weft-runner-github, weft-runner-gitlab,
// weft-runner-forgejo) share the lifecycle layer (anything that talks to
// weft to spawn / drive / tear down a VM); the per-platform code is small
// (each platform's polling protocol + job spec envelope). When the three
// diverge enough to warrant it, the shared "microVM job runtime" should
// split into its own sibling module they all import.
//
// # Status (2026-06)
//
//  1. ✓ GitLab CI runner registration via REST (registerRunner against
//     /api/v4/runners). PersistedConfig stores the resulting runner
//     token + ID for the long-poll loop.
//  2. ✓ Runner-config persistence + ephemeral-runner semantics
//     (PersistedConfig + per-job request job semantics on the in-VM
//     gitlab-runner binary).
//  3. ✓ Long-poll loop : Run dispatches concurrent workers ; each polls
//     POST /api/v4/jobs/request and dispatches the assigned job to a
//     fresh microVM.
//  4. ✓ weft microVM spawn via dispatchJob → weft-client RegisterMicroVM.
//  5. ✓ In-VM agent : the runner image ships gitlab-runner exec mode
//     reading the per-job spec from /run/weft/cfg/. Image side ; this
//     daemon only puts the file there via the share.
//  6. ✓ Log streaming : the in-VM gitlab-runner ships logs to GitLab via
//     the /api/v4/jobs/{id}/trace endpoint directly.
//  7. ✓ Cleanup on cancel + idle timeout : worker goroutines honour ctx.
//
// All seven items shipped. Subsequent work focuses on observability
// (per-job timing, queue-depth metrics) and on the shared microVM-job
// runtime split mentioned above — neither is functional surface.
package main
