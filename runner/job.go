// runner/job.go — per-job microVM lifecycle for GitLab CI.
//
// Same shape as the github sibling — shell out to `weft microvm …` for the
// VM moves. The difference is the in-VM contract: GitLab CI jobs are bash
// scripts executed against an image specified by `image: ...` in
// .gitlab-ci.yml. The runner builds a small "step script" that the in-VM
// agent reads off the `cfg` share.
//
// Trace shipping path: instead of `weft microvm wait` (which blocks until
// exit and gives us nothing while the job runs), we pipe `weft microvm
// logs --follow` through a buffered reader that PATCHes /trace every
// flushInterval or whenever the buffer crosses flushBytes — whichever
// hits first. The wait completes when the logs stream EOFs (init exits
// → logs follow ends).

package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// Trace shipping cadence. Chosen to balance GitLab UI freshness (operators
// expect to see logs scroll within a couple of seconds) against PATCH
// volume — every PATCH is a round-trip, batching reduces wear on the
// GitLab API token's rate limit.
const (
	traceFlushInterval = 2 * time.Second
	traceFlushBytes    = 8 * 1024
)

// dispatchJob runs one GitLab job in a fresh microVM. See file doc.
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

	// Stream logs → trace. The function blocks until the logs stream EOFs
	// (which happens when the VM's init exits, i.e. the job is done).
	// streamErr is non-nil only on a transport-level failure of the logs
	// subprocess; a script that exits non-zero produces logs cleanly and
	// then EOFs — we detect the failure separately via `weft microvm
	// wait` exit status.
	streamErr := streamLogsToTrace(ctx, g, spec, weftEndpoint, vmName)
	if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
		log.Printf("weft-runner-gitlab: log stream warning: %v", streamErr)
	}

	// `weft microvm wait` should return immediately now that the logs
	// stream ended, but call it anyway so we observe the exit status
	// rather than guessing from log content.
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

// streamLogsToTrace pipes `weft microvm logs --follow <vm>` through a
// buffered reader and PATCHes the GitLab trace in chunks. Returns when:
//   - the logs subprocess EOFs (init exited inside the VM); OR
//   - ctx is cancelled (operator SIGTERM); OR
//   - a PATCH /trace returns a hard error (rare — GitLab is lenient
//     about /trace, even a stale offset just gets re-tried).
//
// Concurrency model: one reader goroutine fills a ring; one flusher
// timer (in this goroutine) drains it on interval or size threshold.
// Both share `buf` under `mu`. The reader is the only writer to `eof`.
func streamLogsToTrace(ctx context.Context, g *gl, spec *JobSpec, weftEndpoint, vmName string) error {
	endpointFlag := "--endpoint=" + weftEndpoint
	cmd := exec.CommandContext(ctx, "weft", "microvm", "logs", endpointFlag, "--follow", "--name="+vmName)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("logs pipe: %w", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start logs: %w", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	var (
		mu     sync.Mutex
		buf    []byte
		eof    = false
		cursor int64
	)
	// Reader goroutine. bufio.Reader gives us pre-built line buffering so
	// we don't burn syscalls on every byte the VM writes.
	br := bufio.NewReader(stdout)
	go func() {
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				mu.Lock()
				buf = append(buf, line...)
				mu.Unlock()
			}
			if err != nil {
				mu.Lock()
				eof = true
				mu.Unlock()
				return
			}
		}
	}()

	// Flush helper, called on interval / threshold / final EOF.
	flush := func() error {
		mu.Lock()
		if len(buf) == 0 {
			mu.Unlock()
			return nil
		}
		chunk := buf
		buf = nil
		mu.Unlock()
		if err := g.patchTrace(ctx, spec.ID, spec.Token, chunk, cursor); err != nil {
			// On failure, put the chunk back at the front so the
			// next flush retries. GitLab is idempotent on Content-
			// Range, so the worst case is duplicate bytes (the API
			// dedups on offset).
			mu.Lock()
			buf = append(chunk, buf...)
			mu.Unlock()
			return err
		}
		cursor += int64(len(chunk))
		return nil
	}

	t := time.NewTicker(traceFlushInterval)
	defer t.Stop()
	for {
		mu.Lock()
		size := len(buf)
		done := eof
		mu.Unlock()
		if size >= traceFlushBytes {
			if err := flush(); err != nil {
				log.Printf("weft-runner-gitlab: trace flush warning: %v (will retry)", err)
			}
			continue
		}
		if done {
			// Final drain — best-effort.
			_ = flush()
			return nil
		}
		select {
		case <-ctx.Done():
			_ = flush()
			return ctx.Err()
		case <-t.C:
			if err := flush(); err != nil {
				log.Printf("weft-runner-gitlab: trace flush warning: %v (will retry)", err)
			}
		}
	}
}

// streamLogsToTrace_io is kept as a doc anchor for the io.Reader-only
// alternative we considered — using io.TeeReader to fork stdout into both
// the GitLab trace and the daemon's stderr. We didn't go that way because
// PATCH /trace is rate-bound and forwarding every read syscall would
// quintuple the API call count for verbose jobs. The buffered version
// above is the right shape.
var _ io.Reader = (*bufio.Reader)(nil)
