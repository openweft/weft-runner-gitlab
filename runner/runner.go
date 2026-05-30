// Package runner is the daemon side of weft-runner-gitlab.
//
// One paragraph: Register exchanges a registration token (created by an op
// on the GitLab UI under Settings → CI/CD → Runners) for a long-lived
// runner token, persisting it to disk. Run loads that token, dials weft,
// then enters the canonical GitLab Runner long-poll loop: every few seconds
// POST /api/v4/jobs/request, dispatch any returned job into a fresh
// microVM via job.go, repeat. On SIGTERM the daemon drains in-flight jobs
// and deregisters.

package runner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// RegisterOptions are the inputs to `weft-runner-gitlab register`.
type RegisterOptions struct {
	URL               string
	RegistrationToken string
	Description       string
	Tags              []string
	RunUntagged       bool
	Locked            bool
	ConfigFile        string
}

// Register exchanges a registration token for a runner token.
func Register(opts RegisterOptions) error {
	if opts.URL == "" || opts.RegistrationToken == "" {
		return errors.New("register: --url and --registration-token are required")
	}
	if opts.ConfigFile == "" {
		return errors.New("register: --config is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	g := newGL(opts.URL)
	info := selfInfo()
	resp, err := g.registerRunner(ctx, registerRequest{
		Token:       opts.RegistrationToken,
		Description: opts.Description,
		TagList:     opts.Tags,
		RunUntagged: opts.RunUntagged,
		Locked:      opts.Locked,
		Info:        &info,
	})
	if err != nil {
		return fmt.Errorf("gitlab register: %w", err)
	}
	cfg := PersistedConfig{
		URL:         opts.URL,
		RunnerID:    resp.ID,
		Token:       resp.Token,
		Description: opts.Description,
		Tags:        opts.Tags,
	}
	if err := writeConfig(opts.ConfigFile, cfg); err != nil {
		return err
	}
	log.Printf("weft-runner-gitlab register: id=%d, config %s", resp.ID, opts.ConfigFile)
	return nil
}

// RunOptions configures the long-lived daemon loop.
type RunOptions struct {
	ConfigFile   string
	WeftEndpoint string
	Image        string
	Concurrency  int
	PollInterval int
}

// Run boots the daemon and serves jobs until ctx is cancelled.
func Run(ctx context.Context, opts RunOptions) error {
	if opts.ConfigFile == "" || opts.WeftEndpoint == "" || opts.Image == "" {
		return errors.New("run: --config, --weft-endpoint, --image are required")
	}
	cfg, err := readConfig(opts.ConfigFile)
	if err != nil {
		return err
	}
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	pollInterval := time.Duration(opts.PollInterval) * time.Second
	if pollInterval == 0 {
		pollInterval = 3 * time.Second
	}

	g := newGL(cfg.URL)
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	log.Printf("weft-runner-gitlab run: id=%d url=%s concurrency=%d image=%s",
		cfg.RunnerID, cfg.URL, concurrency, opts.Image)

	backoff := pollInterval
loop:
	for {
		if ctx.Err() != nil {
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		spec, err := g.requestJob(ctx, cfg.Token)
		if err != nil {
			<-sem
			if ctx.Err() != nil {
				break
			}
			log.Printf("weft-runner-gitlab: poll error: %v — retrying in %s", err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				break loop
			}
			if backoff < 60*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = pollInterval
		if spec == nil {
			<-sem
			select {
			case <-time.After(pollInterval):
			case <-ctx.Done():
				break loop
			}
			continue
		}
		wg.Add(1)
		go func(s *JobSpec) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := dispatchJob(ctx, g, opts.WeftEndpoint, opts.Image, s); err != nil {
				log.Printf("weft-runner-gitlab: job %d dispatch error: %v", s.ID, err)
			}
		}(spec)
	}

	log.Printf("weft-runner-gitlab: ctx cancelled, draining %d in-flight job(s)", len(sem))
	wg.Wait()

	dctx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dcancel()
	if err := g.unregisterRunner(dctx, cfg.Token); err != nil {
		log.Printf("weft-runner-gitlab: deregister warning: %v", err)
	}
	return ctx.Err()
}
