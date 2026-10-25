# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Real script extraction for GitLab `JobSpec.steps` and in-VM exec wiring.

### Changed

- In-VM image: gitlab-runner is pinned to v19.5.1 (`GITLAB_RUNNER_VERSION`
  build argument) instead of the unpinned `latest` channel, so published
  images stop carrying a stale or arbitrary toolchain.

## [v0.1.0] — 2026-05-30

### Added

- Initial skeleton: Go module, CLI, runner package boundaries.
- Real REST integration — `Register` exchanges a token, `Run` long-polls jobs.
- In-VM GitLab runner image plus REST shim tests.
- Streaming of microVM logs to GitLab `/trace` while the job runs.
- CI: build + test on push/PR across linux amd64+arm64 matrix.
