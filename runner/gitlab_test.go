package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSelfInfo(t *testing.T) {
	t.Parallel()
	info := selfInfo()
	if info.Name != "weft-runner-gitlab" {
		t.Errorf("Name = %q, want weft-runner-gitlab", info.Name)
	}
	if info.Executor != "weft-microvm" {
		t.Errorf("Executor = %q, want weft-microvm", info.Executor)
	}
	if info.Platform == "" || info.Architecture == "" || info.Version == "" {
		t.Errorf("selfInfo() has empty fields: %+v", info)
	}
}

func TestRegisterRunner_HappyPath(t *testing.T) {
	t.Parallel()
	var got registerRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v4/runners" {
			t.Errorf("path = %s, want /api/v4/runners", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":42,"token":"runner-token-xyz"}`)
	}))
	defer srv.Close()

	g := newGL(srv.URL)
	in := registerRequest{
		Token:       "reg-token",
		Description: "test",
		TagList:     []string{"linux", "weft"},
		RunUntagged: true,
		Info:        ptrRunnerInfo(selfInfo()),
	}
	resp, err := g.registerRunner(context.Background(), in)
	if err != nil {
		t.Fatalf("registerRunner: %v", err)
	}
	if resp.ID != 42 || resp.Token != "runner-token-xyz" {
		t.Errorf("resp = %+v, want id=42 token=runner-token-xyz", resp)
	}
	if got.Token != "reg-token" {
		t.Errorf("body.Token = %q, want reg-token", got.Token)
	}
	if got.Description != "test" {
		t.Errorf("body.Description = %q, want test", got.Description)
	}
	if len(got.TagList) != 2 || got.TagList[0] != "linux" || got.TagList[1] != "weft" {
		t.Errorf("body.TagList = %v, want [linux weft]", got.TagList)
	}
	if !got.RunUntagged {
		t.Error("body.RunUntagged = false, want true")
	}
	if got.Info == nil || got.Info.Executor != "weft-microvm" {
		t.Errorf("body.Info = %+v, want non-nil with weft-microvm executor", got.Info)
	}
}

func TestRegisterRunner_404Errors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"404 Not Found"}`)
	}))
	defer srv.Close()

	g := newGL(srv.URL)
	_, err := g.registerRunner(context.Background(), registerRequest{Token: "x"})
	if err == nil {
		t.Fatal("registerRunner: want error on 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q does not mention 404", err.Error())
	}
}

func TestRegisterRunner_MissingTokenInResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":7}`)
	}))
	defer srv.Close()

	g := newGL(srv.URL)
	_, err := g.registerRunner(context.Background(), registerRequest{Token: "x"})
	if err == nil || !strings.Contains(err.Error(), "missing token") {
		t.Fatalf("want missing-token error, got %v", err)
	}
}

func TestUnregisterRunner_HappyPath(t *testing.T) {
	t.Parallel()
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/api/v4/runners" {
			t.Errorf("path = %s, want /api/v4/runners", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var body struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotToken = body.Token
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	g := newGL(srv.URL)
	if err := g.unregisterRunner(context.Background(), "rt-1"); err != nil {
		t.Fatalf("unregisterRunner: %v", err)
	}
	if gotToken != "rt-1" {
		t.Errorf("server saw token = %q, want rt-1", gotToken)
	}
}

func TestUnregisterRunner_404Idempotent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	g := newGL(srv.URL)
	// 404 means "already gone" — DELETE must succeed idempotently.
	if err := g.unregisterRunner(context.Background(), "rt-1"); err != nil {
		t.Fatalf("unregisterRunner on 404: want nil, got %v", err)
	}
}

func TestRequestJob_201ReturnsSpec(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v4/jobs/request" {
			t.Errorf("path = %s, want /api/v4/jobs/request", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var body struct {
			Token string     `json:"token"`
			Info  runnerInfo `json:"info"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Token != "rt-runner" {
			t.Errorf("body.Token = %q, want rt-runner", body.Token)
		}
		if body.Info.Executor != "weft-microvm" {
			t.Errorf("body.Info.Executor = %q, want weft-microvm", body.Info.Executor)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":123,"token":"job-tok","image":{"name":"alpine:3"}}`)
	}))
	defer srv.Close()

	g := newGL(srv.URL)
	spec, err := g.requestJob(context.Background(), "rt-runner")
	if err != nil {
		t.Fatalf("requestJob: %v", err)
	}
	if spec == nil {
		t.Fatal("spec = nil, want non-nil")
	}
	if spec.ID != 123 || spec.Token != "job-tok" || spec.Image.Name != "alpine:3" {
		t.Errorf("spec = %+v, want id=123 token=job-tok image=alpine:3", spec)
	}
}

func TestRequestJob_204Idle(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	g := newGL(srv.URL)
	spec, err := g.requestJob(context.Background(), "rt")
	if err != nil {
		t.Fatalf("requestJob 204: err = %v, want nil", err)
	}
	if spec != nil {
		t.Errorf("spec = %+v, want nil on 204", spec)
	}
}

func TestPatchTrace_HappyPath(t *testing.T) {
	t.Parallel()
	type call struct {
		jobToken, contentType, contentRange string
		body                                []byte
	}
	var got call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		if r.URL.Path != "/api/v4/jobs/77/trace" {
			t.Errorf("path = %s, want /api/v4/jobs/77/trace", r.URL.Path)
		}
		got.jobToken = r.Header.Get("JOB-TOKEN")
		got.contentType = r.Header.Get("Content-Type")
		got.contentRange = r.Header.Get("Content-Range")
		got.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	g := newGL(srv.URL)
	chunk := []byte("hello world\n")
	if err := g.patchTrace(context.Background(), 77, "tok", chunk, 100); err != nil {
		t.Fatalf("patchTrace: %v", err)
	}
	if got.jobToken != "tok" {
		t.Errorf("JOB-TOKEN = %q, want tok", got.jobToken)
	}
	if got.contentType != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", got.contentType)
	}
	if got.contentRange != "100-111" {
		t.Errorf("Content-Range = %q, want 100-111", got.contentRange)
	}
	if string(got.body) != "hello world\n" {
		t.Errorf("body = %q, want %q", got.body, "hello world\n")
	}
}

func TestUpdateJob_Success(t *testing.T) {
	t.Parallel()
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/api/v4/jobs/55" {
			t.Errorf("path = %s, want /api/v4/jobs/55", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g := newGL(srv.URL)
	if err := g.updateJob(context.Background(), 55, "jtok", "success", ""); err != nil {
		t.Fatalf("updateJob: %v", err)
	}
	if got["token"] != "jtok" || got["state"] != "success" {
		t.Errorf("body = %v, want token=jtok state=success", got)
	}
	if _, ok := got["failure_reason"]; ok {
		t.Errorf("body should omit failure_reason on success, got %v", got)
	}
}

func TestUpdateJob_FailedWithReason(t *testing.T) {
	t.Parallel()
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g := newGL(srv.URL)
	if err := g.updateJob(context.Background(), 55, "jtok", "failed", "script_failure"); err != nil {
		t.Fatalf("updateJob: %v", err)
	}
	if got["state"] != "failed" || got["failure_reason"] != "script_failure" {
		t.Errorf("body = %v, want state=failed failure_reason=script_failure", got)
	}
}

func TestNewGL_TrimsTrailingSlash(t *testing.T) {
	t.Parallel()
	g := newGL("https://gitlab.example.com/")
	if g.url != "https://gitlab.example.com" {
		t.Errorf("url = %q, want trailing slash trimmed", g.url)
	}
}

// ptrRunnerInfo is a tiny helper because Go has no &literal syntax for
// struct-pointer-out-of-a-value when the value comes from a call.
func ptrRunnerInfo(r runnerInfo) *runnerInfo { return &r }

func TestRenderJobScript_SingleStep(t *testing.T) {
	t.Parallel()
	spec := &JobSpec{
		ID:    1,
		Token: "tok",
		Steps: []JobStep{
			{Name: "script", Script: []string{"echo hello", "make build"}},
		},
	}
	out := renderJobScript(spec)
	wantContains := []string{
		"#!/bin/bash",
		"set -e",
		"# step 0: script (when=on_success allow_failure=false)",
		"if [ \"$__weft_job_failed\" -eq 0 ]; then",
		"echo hello",
		"make build",
		"$ === step: script ===",
		"exit \"$__weft_job_failed\"",
	}
	for _, s := range wantContains {
		if !strings.Contains(out, s) {
			t.Errorf("rendered script missing %q\n--- script ---\n%s", s, out)
		}
	}
}

func TestRenderJobScript_OnFailureCleanup(t *testing.T) {
	t.Parallel()
	spec := &JobSpec{
		ID: 2,
		Steps: []JobStep{
			{Name: "build", Script: []string{"make build"}},
			{Name: "tests", Script: []string{"make test"}, AllowFailure: true},
			{Name: "cleanup_on_fail", Script: []string{"rm -rf tmp/"}, When: "on_failure"},
			{Name: "always", Script: []string{"echo bye"}, When: "always"},
		},
	}
	out := renderJobScript(spec)
	// on_failure gate: should branch on prior failure
	if !strings.Contains(out, `# step 2: cleanup_on_fail (when=on_failure`) {
		t.Errorf("missing on_failure step header:\n%s", out)
	}
	if !strings.Contains(out, "if [ \"$__weft_job_failed\" -ne 0 ]; then") {
		t.Errorf("on_failure step does not gate on prior failure:\n%s", out)
	}
	// always step should be unconditional
	if !strings.Contains(out, "# step 3: always (when=always") {
		t.Errorf("missing always step header:\n%s", out)
	}
	if !strings.Contains(out, "if true; then") {
		t.Errorf("always step is not unconditional:\n%s", out)
	}
	// allow_failure step should NOT promote failure
	if !strings.Contains(out, "allow_failure=true, ignored") {
		t.Errorf("allow_failure warning not emitted:\n%s", out)
	}
	// allow_failure must not contain the __weft_job_failed promotion line right after its rc check
	tests := "# step 1: tests (when=on_success allow_failure=true)"
	idx := strings.Index(out, tests)
	if idx < 0 {
		t.Fatalf("missing allow_failure step header:\n%s", out)
	}
	stepTwoIdx := strings.Index(out, "# step 2:")
	if stepTwoIdx < 0 || stepTwoIdx <= idx {
		t.Fatalf("step 2 header not after step 1: %d %d", idx, stepTwoIdx)
	}
	allowFailureBlock := out[idx:stepTwoIdx]
	if strings.Contains(allowFailureBlock, "__weft_job_failed=$__weft_step_rc") {
		t.Errorf("allow_failure step must not promote __weft_job_failed:\n%s", allowFailureBlock)
	}
}

func TestRenderJobScript_VariablesPublicAndMasked(t *testing.T) {
	t.Parallel()
	spec := &JobSpec{
		ID: 3,
		Variables: []JobVariable{
			{Key: "CI_COMMIT_SHA", Value: "abc123", Public: true},
			{Key: "SECRET_TOKEN", Value: "s3cret", Public: false},
		},
		Steps: []JobStep{
			{Name: "noop", Script: []string{"true"}},
		},
	}
	out := renderJobScript(spec)
	// Both variables are exported with real values
	if !strings.Contains(out, "export CI_COMMIT_SHA='abc123'") {
		t.Errorf("public var not exported with literal value:\n%s", out)
	}
	if !strings.Contains(out, "export SECRET_TOKEN='s3cret'") {
		t.Errorf("masked var not exported with literal value (should still export):\n%s", out)
	}
	// Trace echo: public shows value, masked shows [MASKED]
	if !strings.Contains(out, "$ export CI_COMMIT_SHA=abc123") {
		t.Errorf("public var echo does not show value:\n%s", out)
	}
	if !strings.Contains(out, "$ export SECRET_TOKEN=[MASKED]") {
		t.Errorf("masked var echo leaks value or wrong format:\n%s", out)
	}
	if strings.Contains(out, "$ export SECRET_TOKEN=s3cret") {
		t.Errorf("masked var leaked plaintext value in echo:\n%s", out)
	}
}

func TestRenderJobScript_QuotesSafely(t *testing.T) {
	t.Parallel()
	// shQuote must handle embedded single quotes (the value-with-apostrophe
	// case) without producing invalid bash.
	spec := &JobSpec{
		Variables: []JobVariable{
			{Key: "MSG", Value: "it's fine", Public: true},
		},
	}
	out := renderJobScript(spec)
	want := `export MSG='it'\''s fine'`
	if !strings.Contains(out, want) {
		t.Errorf("expected single-quote-escaped export, missing %q:\n%s", want, out)
	}
}
