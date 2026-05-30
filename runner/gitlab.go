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

// JobSpec is a *minimal* projection of the POST /jobs/request payload —
// just what dispatchJob needs to spawn a VM. The real payload is large
// (~30 nested keys covering steps, variables, artifacts, cache, services,
// dependencies, image+services pull policies, secrets). Adding fields here
// is a per-feature exercise; this is intentionally the bare minimum so the
// scaffolding compiles end-to-end and the dispatch path is exercised.
type JobSpec struct {
	ID    int64  `json:"id"`
	Token string `json:"token"` // job-token used for trace + update
	Image struct {
		Name string `json:"name"`
	} `json:"image"`
	Variables []struct {
		Key    string `json:"key"`
		Value  string `json:"value"`
		Public bool   `json:"public"`
	} `json:"variables"`
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
