// runner/job.go — per-job microVM lifecycle for GitLab CI.
//
// Same shape as the github sibling — shell out to `weft microvm …` for the
// VM moves. The difference is the in-VM contract: GitLab CI jobs are bash
// scripts executed against an image specified by `image: ...` in
// .gitlab-ci.yml. The runner builds a small "step script" that the in-VM
// agent reads off the `cfg` share.
//
// This commit lands the lifecycle plumbing (mktemp + cfg share + weft moves
// + trace patching skeleton) but leaves the actual script generation as a
// TODO — that needs JobSpec to gain the script/variables/services fields,
// which is meaningful follow-on work.

package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// dispatchJob runs one GitLab job in a fresh microVM:
//
//  1. Materialise a temp cfg dir with gitlab-job.json (the JobSpec serialised
//     as the in-VM agent reads it).
//  2. `weft microvm register` with cfg mounted at /run/weft/cfg.
//  3. `weft microvm start` then `wait` for the VM to terminate.
//  4. PATCH trace + PUT state back to GitLab.
//  5. `weft microvm delete` (deferred, idempotent).
//
// Errors are propagated as the GitLab job's failure reason. dispatchJob
// itself returning nil means the *lifecycle* succeeded — the job's
// success/failure is reported via updateJob inside this function.
func dispatchJob(ctx context.Context, g *gl, weftEndpoint, image string, spec *JobSpec) error {
	vmName := fmt.Sprintf("gitlab-job-%d", spec.ID)
	cfgDir, err := os.MkdirTemp("", "weft-runner-gitlab-"+vmName+"-cfg-")
	if err != nil {
		return fmt.Errorf("mktemp cfg: %w", err)
	}
	defer os.RemoveAll(cfgDir)

	specBytes, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshal job spec: %w", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "gitlab-job.json"), specBytes, 0o600); err != nil {
		return fmt.Errorf("write gitlab-job.json: %w", err)
	}

	// Effective image: per-job override (`.gitlab-ci.yml: image:`) takes
	// precedence over the daemon's --image default. Mirrors the gitlab-
	// runner docker executor's resolution.
	jobImage := image
	if spec.Image.Name != "" {
		jobImage = spec.Image.Name
	}

	endpointFlag := "--endpoint=" + weftEndpoint
	register := exec.CommandContext(ctx, "weft", "microvm", "register",
		endpointFlag,
		"--name="+vmName,
		"--image="+jobImage,
		"--cfg="+cfgDir,
	)
	register.Stdout = os.Stderr
	register.Stderr = os.Stderr
	if err := register.Run(); err != nil {
		_ = g.updateJob(ctx, spec.ID, spec.Token, "failed", "runner_system_failure")
		return fmt.Errorf("weft microvm register: %w", err)
	}
	defer func() {
		delCtx, delCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer delCancel()
		del := exec.CommandContext(delCtx, "weft", "microvm", "delete", endpointFlag, "--name="+vmName)
		del.Stderr = os.Stderr
		if err := del.Run(); err != nil {
			log.Printf("weft-runner-gitlab: delete %s failed: %v (leaked weft-side VM)", vmName, err)
		}
	}()

	start := exec.CommandContext(ctx, "weft", "microvm", "start", endpointFlag, "--name="+vmName)
	start.Stderr = os.Stderr
	if err := start.Run(); err != nil {
		_ = g.updateJob(ctx, spec.ID, spec.Token, "failed", "runner_system_failure")
		return fmt.Errorf("weft microvm start: %w", err)
	}

	// TODO(milestone-trace): tail the microVM's stdout (`weft microvm
	// logs --follow`) and PATCH the trace every few KB. For now we
	// post a marker so the GitLab UI shows *something* even if the
	// in-VM script writes nothing.
	if err := g.patchTrace(ctx, spec.ID, spec.Token,
		[]byte(fmt.Sprintf("[weft] microVM %s started; trace streaming not yet implemented\n", vmName)), 0); err != nil {
		log.Printf("weft-runner-gitlab: initial trace PATCH failed: %v", err)
	}

	wait := exec.CommandContext(ctx, "weft", "microvm", "wait", endpointFlag, "--name="+vmName)
	wait.Stderr = os.Stderr
	waitErr := wait.Run()

	state, reason := "success", ""
	if waitErr != nil {
		state, reason = "failed", "script_failure"
		log.Printf("weft-runner-gitlab: vm %s wait error: %v → marking job failed", vmName, waitErr)
	}
	if err := g.updateJob(ctx, spec.ID, spec.Token, state, reason); err != nil {
		log.Printf("weft-runner-gitlab: updateJob: %v", err)
	}
	return nil
}
