// Command weft-runner-gitlab wires the cobra entry point. The daemon /
// lifecycle code lives in runner/, the GitLab REST integration in
// runner/gitlab.go, and the per-job microVM logic in runner/job.go. See
// doc.go for the design intent.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/openweft/weft-runner-gitlab/runner"
	"github.com/spf13/cobra"
)

func main() {
	root := &cobra.Command{
		Use:   "weft-runner-gitlab",
		Short: "Self-hosted GitLab CI runner backed by weft ephemeral microVMs",
	}
	root.AddCommand(registerCmd(), runCmd())
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// registerCmd exchanges a GitLab registration token (group/project) for a
// long-lived runner token and persists it to disk. The op creates the
// registration token in the GitLab UI under Settings → CI/CD → Runners.
func registerCmd() *cobra.Command {
	var url, regToken, description, configFile string
	var tags []string
	var runUntagged, locked bool
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Exchange a GitLab registration token for a runner token and persist it",
		Long: `Calls POST /api/v4/runners with the registration token you obtained from
the GitLab UI (Settings → CI/CD → Runners) and writes the returned long-lived
runner token to --config. ` + "`run`" + ` reads that config to long-poll for jobs.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runner.Register(runner.RegisterOptions{
				URL:               url,
				RegistrationToken: regToken,
				Description:       description,
				Tags:              tags,
				RunUntagged:       runUntagged,
				Locked:            locked,
				ConfigFile:        configFile,
			})
		},
	}
	cmd.Flags().StringVar(&url, "url", "https://gitlab.com", "GitLab base URL (self-hosted: e.g. https://gitlab.example.com)")
	cmd.Flags().StringVar(&regToken, "registration-token", "", "Registration token obtained from the GitLab UI (required)")
	cmd.Flags().StringVar(&description, "description", "weft microVM runner", "Description shown in the GitLab runner UI")
	cmd.Flags().StringSliceVar(&tags, "tags", []string{"weft", "microvm"}, "Tags exposed to .gitlab-ci.yml job tags filters")
	cmd.Flags().BoolVar(&runUntagged, "run-untagged", true, "Pick up jobs that have no tags filter")
	cmd.Flags().BoolVar(&locked, "locked", false, "Lock the runner to its current project (no other projects may use it)")
	cmd.Flags().StringVar(&configFile, "config", "weft-runner-gitlab.json", "Path to write the persisted runner config")
	return cmd
}

// runCmd boots the long-lived daemon. SIGTERM/SIGINT trigger context
// cancellation so the runner drains in-flight jobs and deregisters cleanly.
func runCmd() *cobra.Command {
	var configFile, weftEndpoint, image string
	var concurrency, pollInterval int
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Long-lived runner loop — poll GitLab, dispatch jobs into microVMs",
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return runner.Run(ctx, runner.RunOptions{
				ConfigFile:   configFile,
				WeftEndpoint: weftEndpoint,
				Image:        image,
				Concurrency:  concurrency,
				PollInterval: pollInterval,
			})
		},
	}
	cmd.Flags().StringVar(&configFile, "config", "weft-runner-gitlab.json", "Runner config written by `register`")
	cmd.Flags().StringVar(&weftEndpoint, "weft-endpoint", "", "weft control-plane target — unix:/path or tcp:host:port (required)")
	cmd.Flags().StringVar(&image, "image", "", "OCI image ref used as the per-job microVM rootfs fallback (required)")
	cmd.Flags().IntVar(&concurrency, "concurrency", 1, "Maximum in-flight jobs (microVMs)")
	cmd.Flags().IntVar(&pollInterval, "poll-interval", 3, "Seconds between /jobs/request long-polls when idle")
	return cmd
}
