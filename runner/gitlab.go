// Package runner — GitLab REST shim.
//
// GitLab Runner uses a much simpler protocol than GitHub Actions: a single
// REST API speaks both registration and the per-job loop. The endpoints we
// need are:
//
//   - POST /api/v4/runners                         — exchange a registration
//                                                    token (group/project-
//                                                    scoped) for a long-lived
//                                                    runner token.
//   - DELETE /api/v4/runners/<id>                  — deregister.
//   - POST /api/v4/jobs/request                    — long-poll. 201 = job
//                                                    assigned (body = JobSpec),
//                                                    204 = nothing to do.
//   - PATCH /api/v4/jobs/<id>/trace                — append log lines to the
//                                                    live job trace.
//   - PUT /api/v4/jobs/<id>                        — update job state
//                                                    (running → success /
//                                                    failed / canceled).
//
// All four endpoints accept the runner token via either:
//
//   - The `JOB-TOKEN` header (job-scoped operations: trace + update).
//   - A `token=<…>` form/JSON field (long-poll, register, deregister).
//
// We use header-or-body whichever is canonical for each call. The official
// `gitlab-org/gitlab-runner` Go client does the same.
//
// We deliberately do NOT carry a GitLab SDK dependency. The endpoints are
// stable and few; pulling in `gitlab.com/gitlab-org/api/client-go` would
// bring in a transitive graph (logrus + a lot more) for ~200 lines worth of
// shim.

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// gl wraps the few REST calls we need against a single GitLab URL.
type gl struct {
	client *http.Client
	url    string // base URL, e.g. https://gitlab.com (no trailing slash)
}

func newGL(baseURL string) *gl {
	return &gl{
		client: &http.Client{Timeout: 60 * time.Second},
		url:    strings.TrimRight(baseURL, "/"),
	}
}

// regResponse mirrors the POST /api/v4/runners 201 payload (subset).
type regResponse struct {
	ID    int    `json:"id"`
	Token string `json:"token"` // long-lived runner token
}

// registerRequest is the multi-field body POST /api/v4/runners expects.
// `info` carries the runner's self-description; GitLab uses it to populate
// the runner detail page. We send a minimal-but-honest set.
type registerRequest struct {
	Token           string   `json:"token"`             // *registration* token (group/project)
	Description     string   `json:"description,omitempty"`
	TagList         []string `json:"tag_list,omitempty"`
	RunUntagged     bool     `json:"run_untagged,omitempty"`
	Locked          bool     `json:"locked,omitempty"`
	AccessLevel     string   `json:"access_level,omitempty"` // "not_protected" | "ref_protected"
	MaximumTimeout  int      `json:"maximum_timeout,omitempty"`
	Info            *runnerInfo `json:"info,omitempty"`
}

type runnerInfo struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	Revision     string `json:"revision"`
	Platform     string `json:"platform"`
	Architecture string `json:"architecture"`
	Executor     string `json:"executor"`
}

// registerRunner exchanges a registration token for a long-lived runner
// token. The returned token is the credential the daemon uses for every
// subsequent /api/v4/jobs/request call.
func (g *gl) registerRunner(ctx context.Context, req registerRequest) (regResponse, error) {
	var out regResponse
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.url+"/api/v4/runners", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	resp, err := g.client.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return out, fmt.Errorf("gitlab POST /runners: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return out, fmt.Errorf("decode register response: %w", err)
	}
	if out.Token == "" {
		return out, errors.New("gitlab POST /runners: response missing token")
	}
	return out, nil
}

// unregisterRunner deletes a runner by its runner-token. Idempotent on
// the GitLab side — a 404 means already gone.
func (g *gl) unregisterRunner(ctx context.Context, runnerToken string) error {
	body, err := json.Marshal(struct {
		Token string `json:"token"`
	}{runnerToken})
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, g.url+"/api/v4/runners", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("gitlab DELETE /runners: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
}

// JobSpec is a projection of the POST /jobs/request payload — the subset
// the daemon needs to spawn a VM and render its in-VM script. The full
// payload is much larger (~30 nested keys covering artifacts, cache,
// services, dependencies, pull policies, secrets). What we carry here is
// what `renderJobScript` consumes: id+token, image, variables, and the
// flattened `steps` array GitLab assembles from .gitlab-ci.yml's
// before_script + script + after_script.
type JobSpec struct {
	ID    int64     `json:"id"`
	Token string    `json:"token"` // job-token used for trace + update
	Image JobImage  `json:"image"`
	Variables []JobVariable `json:"variables"`
	Steps     []JobStep     `json:"steps"`
}

// JobImage is GitLab's `image:` block. Entrypoint is honoured by the
// upstream gitlab-runner when running in container executors; in our
// microVM context the rootfs starts via weft-init → runner-init, so
// Entrypoint is informational only.
type JobImage struct {
	Name       string   `json:"name"`
	Entrypoint []string `json:"entrypoint,omitempty"`
}

// JobVariable is one entry of the .variables[] block. Public=false means
// the value is masked in the rendered trace echo (GitLab's "masked"
// variables flag); the actual export still happens — the masking is only
// in the logged echo header.
type JobVariable struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Public bool   `json:"public"`
}

// JobStep is one step of the assembled execution plan. GitLab assembles
// before_script + script + after_script into a flat .steps[] array; each
// entry carries its own `when` predicate and `allow_failure` flag.
//
//   When ∈ {"on_success", "on_failure", "always"} — default on_success.
//   AllowFailure: a non-zero exit must not fail the overall job.
type JobStep struct {
	Name         string   `json:"name"`
	Script       []string `json:"script"`
	When         string   `json:"when,omitempty"`
	AllowFailure bool     `json:"allow_failure,omitempty"`
	Timeout      int      `json:"timeout,omitempty"`
}

// renderJobScript turns a JobSpec into a bash script the in-VM agent
// executes verbatim. The output is intentionally readable — every step
// emits an `echo` header so the GitLab trace UI mirrors the upstream
// gitlab-runner's "Running with…" framing.
//
// Semantics, mirroring the upstream runner:
//
//   - `set -e` is the default. Each step runs with -e so the first
//     failing line aborts the step.
//   - A step with `when: on_failure` runs only if a prior step failed; it
//     executes under `set +e` so it can clean up best-effort.
//   - A step with `when: always` runs regardless of prior failure
//     (typical for after_script).
//   - `allow_failure: true` means the step's non-zero exit does NOT
//     promote the overall job to failed.
//   - Variables are exported in a single block at the top. Non-Public
//     variables are still exported but their value is replaced by
//     `[MASKED]` in the trace-echo header so secrets don't leak via the
//     trace stream. (GitLab masks separately on its side; we belt-and-
//     braces the local echo too.)
func renderJobScript(spec *JobSpec) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("# Generated by weft-runner-gitlab. Do not edit.\n")
	b.WriteString("set -e\n")
	b.WriteString("set -o pipefail\n")
	b.WriteString("\n")
	b.WriteString("# --- variables ---\n")
	for _, v := range spec.Variables {
		disp := v.Value
		if !v.Public {
			disp = "[MASKED]"
		}
		fmt.Fprintf(&b, "echo %s\n", shQuote(fmt.Sprintf("$ export %s=%s", v.Key, disp)))
		fmt.Fprintf(&b, "export %s=%s\n", v.Key, shQuote(v.Value))
	}
	b.WriteString("\n")
	b.WriteString("# --- steps ---\n")
	b.WriteString("__weft_job_failed=0\n")
	for i, step := range spec.Steps {
		when := step.When
		if when == "" {
			when = "on_success"
		}
		name := step.Name
		if name == "" {
			name = fmt.Sprintf("step_%d", i)
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "# step %d: %s (when=%s allow_failure=%t)\n", i, name, when, step.AllowFailure)
		// Gate the step on prior failure status per `when`.
		switch when {
		case "on_failure":
			b.WriteString("if [ \"$__weft_job_failed\" -ne 0 ]; then\n")
		case "always":
			b.WriteString("if true; then\n")
		default: // on_success
			b.WriteString("if [ \"$__weft_job_failed\" -eq 0 ]; then\n")
		}
		fmt.Fprintf(&b, "  echo %s\n", shQuote(fmt.Sprintf("$ === step: %s ===", name)))
		// on_failure cleanup steps run best-effort.
		if when == "on_failure" {
			b.WriteString("  set +e\n")
		} else {
			b.WriteString("  set -e\n")
		}
		b.WriteString("  (\n")
		for _, line := range step.Script {
			fmt.Fprintf(&b, "    echo %s\n", shQuote("$ "+line))
			b.WriteString("    " + line + "\n")
		}
		b.WriteString("  )\n")
		b.WriteString("  __weft_step_rc=$?\n")
		if step.AllowFailure {
			fmt.Fprintf(&b, "  if [ \"$__weft_step_rc\" -ne 0 ]; then echo %s; fi\n",
				shQuote(fmt.Sprintf("warning: step %s failed (allow_failure=true, ignored)", name)))
		} else if when != "on_failure" {
			b.WriteString("  if [ \"$__weft_step_rc\" -ne 0 ]; then __weft_job_failed=$__weft_step_rc; fi\n")
		}
		b.WriteString("fi\n")
	}
	b.WriteString("\n")
	b.WriteString("exit \"$__weft_job_failed\"\n")
	return b.String()
}

// shQuote returns s wrapped in single-quotes safe for inclusion in a
// POSIX shell command. We use the canonical `'\''` trick rather than
// %q's Go-style escapes because the latter is interpreted by bash as
// double-quoted ANSI-C escapes and would mangle e.g. literal backslashes
// in script lines.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// requestJob long-polls for a job assignment. Returns (nil, nil) when no
// job is available (HTTP 204) — that's the normal idle path, callers should
// back off and retry. Returns a non-nil JobSpec on HTTP 201.
//
// GitLab's "long-poll" semantics: the server responds within a few seconds
// regardless, the client is expected to call back immediately. We do not
// keep a persistent connection — that's a SaaS feature that requires a
// websocket upgrade we don't need.
func (g *gl) requestJob(ctx context.Context, runnerToken string) (*JobSpec, error) {
	body, _ := json.Marshal(struct {
		Token string     `json:"token"`
		Info  runnerInfo `json:"info"`
	}{
		Token: runnerToken,
		Info:  selfInfo(),
	})
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, g.url+"/api/v4/jobs/request", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	resp, err := g.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusCreated:
		var js JobSpec
		if err := json.NewDecoder(resp.Body).Decode(&js); err != nil {
			return nil, fmt.Errorf("decode job payload: %w", err)
		}
		return &js, nil
	default:
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("gitlab POST /jobs/request: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}

// patchTrace appends `chunk` to the job's live log trace. GitLab keys
// chunks by byte range, expecting the daemon to keep a per-job cursor.
func (g *gl) patchTrace(ctx context.Context, jobID int64, jobToken string, chunk []byte, startByte int64) error {
	endByte := startByte + int64(len(chunk)) - 1
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		fmt.Sprintf("%s/api/v4/jobs/%d/trace", g.url, jobID), bytes.NewReader(chunk))
	if err != nil {
		return err
	}
	httpReq.Header.Set("JOB-TOKEN", jobToken)
	httpReq.Header.Set("Content-Type", "text/plain")
	httpReq.Header.Set("Content-Range", fmt.Sprintf("%d-%d", startByte, endByte))
	resp, err := g.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gitlab PATCH /jobs/%d/trace: HTTP %d: %s", jobID, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// updateJob transitions a job to its terminal state. `failureReason` is
// only relevant when state="failed"; GitLab accepts "script_failure",
// "runner_system_failure", "job_execution_timeout", "stuck_or_timeout_failure".
func (g *gl) updateJob(ctx context.Context, jobID int64, jobToken, state, failureReason string) error {
	payload := map[string]string{"token": jobToken, "state": state}
	if failureReason != "" {
		payload["failure_reason"] = failureReason
	}
	body, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut,
		fmt.Sprintf("%s/api/v4/jobs/%d", g.url, jobID), bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gitlab PUT /jobs/%d: HTTP %d: %s", jobID, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

// selfInfo is the runner self-description GitLab logs in its UI. Keeping
// the version string distinct from the gitlab-runner upstream avoids
// confusion in support workflows ("which runner is this?").
func selfInfo() runnerInfo {
	return runnerInfo{
		Name:         "weft-runner-gitlab",
		Version:      "0.0.0",
		Revision:     "dev",
		Platform:     "linux",
		Architecture: "amd64",
		Executor:     "weft-microvm",
	}
}
