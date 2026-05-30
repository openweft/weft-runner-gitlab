// runner config persistence. Mirrors the github sibling's layout — same
// rationale (file owns a token, 0600, env-var override for LoadCredential
// workflows). The persisted token here is the *runner* token GitLab returns
// from POST /runners, NOT the *registration* token an operator supplies once.

package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PersistedConfig is the on-disk shape of `weft-runner-gitlab register`.
type PersistedConfig struct {
	URL         string   `json:"url"`           // e.g. https://gitlab.com
	RunnerID    int      `json:"runner_id"`     // returned by POST /runners
	Token       string   `json:"token"`         // runner token, long-lived
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

func writeConfig(path string, cfg PersistedConfig) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func readConfig(path string) (PersistedConfig, error) {
	var cfg PersistedConfig
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w (run `weft-runner-gitlab register` first)", path, err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("decode %s: %w", path, err)
	}
	if v := os.Getenv("WEFT_RUNNER_GITLAB_TOKEN"); v != "" {
		cfg.Token = v
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	if cfg.URL == "" {
		return cfg, fmt.Errorf("config %s missing url", path)
	}
	if cfg.Token == "" {
		return cfg, fmt.Errorf("config %s missing token", path)
	}
	return cfg, nil
}
