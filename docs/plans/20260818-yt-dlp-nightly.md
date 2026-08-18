# Plan: Restore 1080p YouTube downloads with a pinned yt-dlp nightly

## Goal

Fix issue #26 by making the checksum-verified yt-dlp nightly `2026.08.17.073947` the reproducible Sushe service runtime, deployable by the restricted `sushe` operator, while preserving the existing 1080p format selector. The parent supervisor will merge, deploy, restart Sushe, and prove both originally failing videos reach production API `done/ok:true` delivery.

## Non-goals

- No format-18/360p or other quality fallback.
- No Deno, PO-token provider, yt-dlp self-update channel, or broad package upgrade.
- No systemd unit changes, apt installs, unrestricted sudo, or `telegram-bot-api` restart.
- No changes to Instagram cookies, Firefox user agent, throttling, retries, or rate-limit gate.
- No unrelated downloader refactor or generic recovery framework.

## Context

- Production stable `2026.07.04` is already the latest stable but both target videos fail around 10 MiB on `299+140` with HTTP 403.
- Upstream `yt-dlp/yt-dlp#17348` records the August 2026 `android_vr` change after stable.
- A checksum-verified nightly `2026.08.17.073947` downloaded 205,196,806 bytes under the same production arguments without 403; Deno with stable did not help.
- `internal/downloader/downloader.go` invokes bare `yt-dlp` at three sites.
- The service loads `/home/sushe/sushe/.env` from its working directory. The restricted operator can write `/home/sushe/sushe/**` and run only documented allowlisted sudo commands.
- `scripts/update.sh` currently assumes an admin SSH user for binary installation, so the merged Go change also needs an operator-safe repository-owned rollout path.

## Validation Commands

```bash
make test
go vet ./...
go build ./...
bash -n scripts/install-ytdlp.sh scripts/update.sh scripts/deploy.sh scripts/verify.sh
shellcheck scripts/install-ytdlp.sh scripts/update.sh scripts/deploy.sh scripts/verify.sh
```

## Tasks

### Task 1: Route every downloader invocation through an explicit yt-dlp binary

**Goal:** Let the service select an operator-owned yt-dlp executable without changing PATH or the systemd unit.

**Serves:** The operator requested an yt-dlp update that restores 1080p downloads, while production permissions prohibit replacing the root-owned stable binary.

- [ ] Add one normalized yt-dlp executable field/helper in `internal/downloader/downloader.go`; empty configuration must retain the current bare `yt-dlp` behavior.
- [ ] Use the configured executable at all three yt-dlp invocation sites without changing format selection, cookies, throttling, or error handling.
- [ ] Thread `SUSHE_YTDLP` from `cmd/sushe/main.go` through `internal/engine` and update internal constructor call sites.
- [ ] Add focused tests for default/trimmed/explicit path behavior and a hermetic stub proving a real yt-dlp call uses the configured executable.
- [ ] Run focused downloader/engine tests and `make test`; commit the completed task.

### Task 2: Add a pinned, checksum-verified, restricted-operator rollout path

**Goal:** Make the verified nightly and merged Sushe binary reproducibly deployable and observable without unrestricted sudo.

**Serves:** The operator required the fix to be deployed and proven, while the current updater hardcodes stable and the current quick-update script assumes admin sudo.

- [ ] Add `scripts/install-ytdlp.sh` with one exact nightly version and SHA-256 pin; verify before remote mutation, install to `/home/<service-user>/sushe/bin/yt-dlp`, atomically preserve all other server `.env` lines while setting `SUSHE_YTDLP`, restart only `sushe`, and verify live env plus exact version.
- [ ] Make `scripts/update.sh` retain its admin mode but add a restricted service-user mode that installs the merged Go binary atomically in the user-owned service directory and uses only the allowlisted Sushe restart.
- [ ] Integrate the pinned installer into the full deploy path without changing the systemd unit or broad host package setup; expose `make update-ytdlp` and label system-vs-service versions in `scripts/verify.sh`.
- [ ] Update `AGENTS.md` for the new env/path, pin-bump workflow, restricted-operator deploy, verification, and rollback; do not expose private host values.
- [ ] Add deterministic script/static tests where practical, run all validation commands, and commit the completed task.

## Parent-owned Post-Completion

- Run the post-Ralphex full-suite gate on the exact final HEAD.
- Open a sanitized PR with `Closes #26`; require green current-head CI and clean review before merge.
- Deploy only merged `origin/main`, install the pinned nightly, restart only `sushe`, and verify live path/version/process/logs.
- Retry the two original videos sequentially through `POST /api/download`; require fresh final NDJSON `done` with `ok:true` for both and retain private message IDs/sizes.
- Verify issue/task closure, final main tests, clean tails, durable Knowledge status, and exactly-once terminal delivery.
