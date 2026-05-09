# yt-dlp Cookies Support

## Overview

Adds optional cookies support to yt-dlp invocations so the bot can authenticate against Instagram (and other platforms that require login). Sourced from environment variable `SUSHE_COOKIES=<path-to-cookies.txt>`. When set, the path is passed as `--cookies <path>` to every yt-dlp call. When unset, behavior is unchanged.

This is the actual fix for the failure pattern surfaced by the previous "yt-dlp stderr error reporting" change. Production logs show ~85% of Instagram failures (`rate-limit reached or login required`, `HTTP 429`, `Login required` for stories, `This content isn't available to everyone`) all turn into the same yt-dlp signal: yt-dlp is anonymous and Instagram won't serve anonymous traffic. Even posts that look public (photo carousels, normal reels) get the same response once the IP rate-limit kicks in. Authenticated cookies bypass all of these.

The cookies file itself is sensitive (auth tokens equivalent to a login session) and lives outside the repo. The plan also tightens `.gitignore` so the cookies file and the operator SSH key (both currently untracked but unprotected) cannot be committed by accident.

**Acceptance criteria:**
- A previously-failing Instagram URL (rate-limit or login-required class) downloads successfully after the cookies file is deployed and the service restarted.
- Bot still starts and processes non-Instagram URLs cleanly when `SUSHE_COOKIES` is unset.
- Both the single-video and playlist code paths use cookies (smoke test must cover both — see Post-Completion).

## Context (from discovery)

- **Files involved**:
  - `internal/downloader/downloader.go` — three yt-dlp invocations need the `--cookies` flag (line numbers approximate; grep `exec.CommandContext.*yt-dlp` for the three sites):
    - `DownloadWithProgress` — single-video download, args built ~lines 132-144
    - `GetPlaylistInfo` — playlist metadata extraction, args built ~lines 388-393
    - `DownloadPlaylistVideo` — specific playlist item, args built ~lines 512-521
  - `internal/engine/engine.go:19-24` — `NewEngine()` constructs the `Downloader`. Cookies path threads through here.
  - `cmd/sushe/main.go:96` — `engine.NewEngine()` call site for the production bot.
  - `cmd/test-split/main.go:53` — direct `downloader.New()` call (utility binary).
  - `internal/api/api_test.go:22, :206` — `engine.NewEngine()` calls in tests.
  - `internal/engine/engine_test.go:180` — `NewEngine()` call in tests.
  - `internal/downloader/downloader_test.go` — existing test file; new tests appended.
  - `.gitignore` — currently does not cover `*cookies*.txt` or `sushe-operator` (SSH private key).
  - `AGENTS.md` — operator workflow docs; needs cookies-deploy step + `SUSHE_COOKIES` listing in env vars.
- **Patterns observed**:
  - Env-var convention `SUSHE_*` already established (`SUSHE_API_TOKEN`, `SUSHE_API_PORT` from existing HTTP API integration).
  - `Downloader` struct (downloader.go:75) currently holds `downloadDir` and `timeout`. New field `cookiesPath` slots in naturally.
  - `Engine` (engine.go) is the layer that owns the `Downloader` lifecycle — env-reading stays in `main`, the constructor takes a plain string path.
- **Production state**: bot is running on `xray:/home/sushe/sushe/bin/sushe` under `sushe.service`. Cookies file `instagram-cookies.txt` is in the local working tree (1767 bytes, untracked). SSH alias `sushe` (was `xray` in `.env`) was added in this session and points at `User=sushe, IdentityFile=~/.ssh/sushe-operator`.
- **Scope**: 4 files modified for code (`downloader.go`, `engine.go`, `cmd/sushe/main.go`, `cmd/test-split/main.go`), 3 test files updated, 1 `.gitignore` line block, 1 docs file. ~60 lines of code total. Manual operator steps for server-side cookies file + systemd env are out-of-codebase and live in Post-Completion.

## Development Approach

- **testing approach**: Regular (code first, then tests, in the same task)
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run `make test` after each change
- non-functional backward compatibility: bot still starts when `SUSHE_COOKIES` is unset (the constructor signatures change, but all in-repo callers are updated in the same task)

## Testing Strategy

- **unit tests**: small helper `cookieArgs(path string) []string` returning `nil` for empty path and `[]string{"--cookies", path}` otherwise. Table-driven test with 2 cases (empty / non-empty). Trivially testable without yt-dlp.
- **wiring verification**: not unit-tested (exec.Cmd is hard to mock). Verified by:
  1. Diff inspection: confirm `cookieArgs(d.cookiesPath)` is prepended at all three yt-dlp arg-builder sites.
  2. Post-deploy smoke test (Post-Completion): exercise **both** the single-video and playlist code paths with Instagram URLs that previously failed. Single video alone is insufficient — `DownloadPlaylistVideo` is a separate site.
- **e2e tests**: project has no e2e harness; not applicable.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

Three layers:

1. **Configuration**: `cmd/sushe/main.go` reads `SUSHE_COOKIES` env var and passes it to `engine.NewEngine(cookiesPath)`. Empty string = disabled (existing behavior).

2. **Plumbing**: New unexported free function `cookieArgs(path string) []string` returns `[]string{"--cookies", path}` when path is non-empty, else `nil`. Free function (not a method) so it tests cleanly without constructing a `Downloader`. The `Downloader` struct gains a `cookiesPath string` field; `engine.NewEngine(cookiesPath string)` threads the path through to `downloader.New(cookiesPath)`. Each of the three yt-dlp arg-builder sites prepends the helper's output to its `args` slice. Prepend (not append) so the URL stays last as required by yt-dlp's expected ordering.

3. **Hygiene**: `.gitignore` gets `*cookies*.txt` and `sushe-operator` so neither the cookies file nor the SSH key can be committed accidentally. Documented operator steps live in `AGENTS.md` (Operator Access section), explicitly noting that systemd drop-in installation is a one-time admin step (not in the operator's routine allowlist).

The bot continues to start and run normally when `SUSHE_COOKIES` is unset — no behavior change for non-Instagram users or for early adopters who deploy before configuring cookies.

## Technical Details

**`cookieArgs(path string) []string`** (new, unexported, free function in `internal/downloader/downloader.go`):
- Input: `path` — absolute path to a Netscape-format cookies file. Empty string = disabled.
- Output: `nil` when empty; `[]string{"--cookies", path}` otherwise (a fresh slice each call, so safe to `append` onto).

**`Downloader.cookiesPath`** (new field):
- Stored at construction time. No runtime mutation. No locking.
- Used only at arg-construction time at the three call sites.

**Signature changes**:
- `downloader.New(cookiesPath string) *Downloader`
- `engine.NewEngine(cookiesPath string) *Engine` — engine threads the value into `downloader.New`.

**Splice form** (identical at all three call sites; concrete example for `DownloadWithProgress`):
```go
args := append(cookieArgs(d.cookiesPath),
    "--no-playlist",
    "-f", "bestvideo[vcodec^=avc1][height<=1080]+bestaudio[acodec^=mp4a]/...",
    "--merge-output-format", "mp4",
    "-o", outputTemplate,
    "--no-warnings",
    "--progress",
    "--newline",
    url,
)
```
- `cookieArgs` returns a fresh slice (not aliased), so this `append` form is safe.
- The URL stays last in the existing arg sequence; cookies prepend is order-safe (yt-dlp accepts options in any position before the URL).

**`.gitignore` additions** (intentionally broad to defend against typo/format variations):
```
# Sensitive operator files
*cookies*.txt
sushe-operator
```

**Server-side bits** (NOT in this PR — manual operator steps; see Post-Completion for full procedure):
- Cookies file deployed to `/home/sushe/.config/sushe/cookies.txt`, owned by `sushe`, mode `0600`.
- systemd drop-in `/etc/systemd/system/sushe.service.d/cookies.conf` with `[Service]\nEnvironment=SUSHE_COOKIES=/home/sushe/.config/sushe/cookies.txt`. Installation requires sudo and is a **one-time admin step**, not part of the routine operator allowlist (per AGENTS.md "Forbidden: Modifying systemd unit files" — drop-ins are arguably out of scope for routine operator action and should be performed by a human admin once).

## What Goes Where

- **Implementation Steps**: code change, helper + tests, gitignore tightening, docs update, plan move. All in-repo.
- **Post-Completion**: scp cookies file to server, install systemd drop-in (one-time admin), restart service, smoke-test single video and playlist Instagram URLs.

## Implementation Steps

### Task 1: Thread cookiesPath through engine → downloader and prepend to all yt-dlp invocations

**Files:**
- Modify: `internal/downloader/downloader.go`
- Modify: `internal/engine/engine.go`
- Modify: `cmd/sushe/main.go`
- Modify: `cmd/test-split/main.go`
- Modify: `internal/api/api_test.go`
- Modify: `internal/engine/engine_test.go`
- Modify: `internal/downloader/downloader_test.go`

- [x] add unexported free function `cookieArgs(path string) []string` to `internal/downloader/downloader.go` near the top of the file
- [x] add `cookiesPath string` field to the `Downloader` struct
- [x] change `New()` signature to `New(cookiesPath string) *Downloader`; populate the new field
- [x] in each of the three yt-dlp invocation sites in `downloader.go` (`DownloadWithProgress`, `GetPlaylistInfo`, `DownloadPlaylistVideo` — grep `exec.CommandContext.*yt-dlp` to locate them), rewrite the `args` slice construction to start with `append(cookieArgs(d.cookiesPath), ...existing args...)` so `--cookies <path>` appears before the URL
- [x] change `engine.NewEngine()` signature to `NewEngine(cookiesPath string) *Engine`; pass `cookiesPath` to `downloader.New(cookiesPath)`
- [x] update `cmd/sushe/main.go:96` to call `engine.NewEngine(os.Getenv("SUSHE_COOKIES"))`
- [x] update `cmd/test-split/main.go:53` to call `downloader.New("")` (utility binary; cookies not needed)
- [x] update `internal/api/api_test.go:22, :206` to call `engine.NewEngine("")`
- [x] update `internal/engine/engine_test.go:180` to call `NewEngine("")`
- [x] add `TestCookieArgs` to `internal/downloader/downloader_test.go` with 2 cases: empty path returns nil, non-empty returns `[]string{"--cookies", path}`
- [x] run `make test` — all existing tests + `TestCookieArgs` must pass
- [x] run `go vet ./... && go build ./...`

### Task 2: Tighten .gitignore for sensitive files

**Files:**
- Modify: `.gitignore`

- [x] append a `# Sensitive operator files` block at end of file with `*cookies*.txt` and `sushe-operator` patterns
- [x] verify with `git check-ignore -v instagram-cookies.txt sushe-operator` that both are now matched (this is the test for this task)

### Task 3: Document operator workflow for cookies deploy

**Files:**
- Modify: `AGENTS.md`

- [x] in the "Environment Variables" → "Optional" list (or equivalent location; AGENTS.md was just refreshed from upstream so verify section names), add `SUSHE_COOKIES=<path>` with a one-line description: "absolute path to Netscape-format cookies file; passed to yt-dlp as `--cookies`"
- [x] in the "Operator Access" section (around lines 280+), add a new subsection "Cookies for authenticated downloads" covering:
  - Why: Instagram (and possibly others) require cookies for most posts; ~85% of Instagram failures observed in production are auth/rate-limit
  - Where the file lives on the server: `/home/sushe/.config/sushe/cookies.txt`, mode `0600`, owned by `sushe`
  - How systemd picks it up: one-time-admin install of drop-in `/etc/systemd/system/sushe.service.d/cookies.conf` (NOT in the routine operator allowlist — an admin sets this up once; thereafter the operator only refreshes the cookies file and restarts the service)
  - Routine operator workflow when the session expires: scp new cookies file with mode 0600, then `sudo systemctl restart sushe`
  - Note: do NOT put `SUSHE_COOKIES` in `.env` — it is set on the server via systemd drop-in. `.env` is for local-machine deploy config (SSH host, etc.).
- [x] **Opportunistic fix-up**: in the same edit, replace residual `sushe-bot` SSH alias references in AGENTS.md (currently lines ~295, ~333, ~342) with `sushe` so docs reflect the alias actually configured in `~/.ssh/config`. Verify with `rg sushe-bot AGENTS.md` returns nothing after the edit.

### Task 4: Verify acceptance criteria

- [x] `make test` green (all existing tests + new `TestCookieArgs`)
- [x] `go vet ./...` clean
- [x] `go build ./...` clean
- [x] read the diff of `internal/downloader/downloader.go` and confirm: every `exec.CommandContext(ctx, "yt-dlp", ...)` site has `cookieArgs(d.cookiesPath)` prepended into its `args` slice (verified: cookieArgs prepended at lines 145, 374, 498 — feeding the three yt-dlp invocations at lines 165, 383, 515)
- [x] confirm `cmd/sushe/main.go` reads `SUSHE_COOKIES` and the bot starts cleanly when it is unset (no panic, no error log) — verify by `go build && SUSHE_COOKIES= go run ./cmd/sushe` smoke locally if possible (or rely on production startup after deploy) (verified: bot runs through env loading and engine construction; only fails on missing local Bot API server, which is expected for local-without-server smoke)
- [x] confirm `git check-ignore -v instagram-cookies.txt sushe-operator` reports both as ignored

### Task 5: Move plan to completed

- [x] move this plan to `docs/plans/completed/20260509-ytdlp-cookies-support.md`

## Post-Completion

**Server-side deployment** (manual, performed once after the PR merges and the binary is deployed via `make update`):

1. Create the cookies directory on the server (idempotent, can re-run safely):
   ```
   ssh sushe "mkdir -p ~/.config/sushe && chmod 700 ~/.config/sushe"
   ```

2. Upload the cookies file (idempotent — overwrites on re-deploy when session expires):
   ```
   scp instagram-cookies.txt sushe:.config/sushe/cookies.txt
   ssh sushe "chmod 600 ~/.config/sushe/cookies.txt"
   ```

3. **One-time admin step** (requires sudo on server): install the systemd drop-in. Per the "Config Change Safety" rule, the procedure is split into substeps so the merged config is verified BEFORE the service is restarted. Use scp + sudo install (not heredoc-over-ssh, which is fragile when stdin is consumed):

   (a) Stage and install the drop-in file on the server:
   ```
   printf '[Service]\nEnvironment=SUSHE_COOKIES=/home/sushe/.config/sushe/cookies.txt\n' > /tmp/cookies.conf
   scp /tmp/cookies.conf sushe:/tmp/cookies.conf
   rm /tmp/cookies.conf
   ssh sushe "sudo mkdir -p /etc/systemd/system/sushe.service.d && sudo install -m 0644 /tmp/cookies.conf /etc/systemd/system/sushe.service.d/cookies.conf && rm /tmp/cookies.conf"
   ```

   (b) Verify the installed drop-in actually contains the expected `Environment=SUSHE_COOKIES=...` line BEFORE proceeding:
   ```
   ssh sushe "cat /etc/systemd/system/sushe.service.d/cookies.conf"
   ```
   Confirm the output contains `Environment=SUSHE_COOKIES=/home/sushe/.config/sushe/cookies.txt`.

   (c1) Reload systemd so it picks up the new drop-in (does NOT restart the running service):
   ```
   ssh sushe "sudo systemctl daemon-reload"
   ```

   (c2) Verify the merged systemd Environment for the unit contains the expected `SUSHE_COOKIES=...` token BEFORE restarting. `systemctl show` reads the merged unit + drop-ins (refreshed by c1), so this works without a restart:
   ```
   ssh sushe "sudo systemctl show sushe -p Environment | grep SUSHE_COOKIES"
   ```
   Confirm the output contains `SUSHE_COOKIES=/home/sushe/.config/sushe/cookies.txt`.

   (c3) Only after (c2) passes, restart the bot so the verified env reaches the running process:
   ```
   ssh sushe "sudo systemctl restart sushe"
   ```

4. Post-restart sanity checks:

   (a) Re-verify the merged unit env (same command as c2, just confirms the drop-in is still installed and parsed correctly post-restart — this does NOT prove the live process picked it up, since `systemctl show -p Environment` reads merged unit properties, not `/proc/<pid>/environ`):
   ```
   ssh sushe "sudo systemctl show sushe -p Environment | grep SUSHE_COOKIES"
   ```
   Expect output containing `SUSHE_COOKIES=/home/sushe/.config/sushe/cookies.txt`.

   (b) Verify the live running process actually has the env var by reading `/proc/<MainPID>/environ`:
   ```
   ssh sushe "sudo cat /proc/\$(systemctl show sushe -p MainPID --value)/environ | tr '\\0' '\\n' | grep SUSHE_COOKIES"
   ```
   Expect output containing `SUSHE_COOKIES=/home/sushe/.config/sushe/cookies.txt`. If (a) passes but (b) fails, the restart didn't take effect (e.g. service failed to start) — investigate with `sudo systemctl status sushe` before proceeding.

5. Smoke-test by sending **two** known-failing URLs to the bot:
   - A single Instagram reel/post that previously hit `rate-limit reached or login required` — exercises `DownloadWithProgress` site.
   - An Instagram playlist URL (saved collection or similar) — exercises `GetPlaylistInfo` and `DownloadPlaylistVideo` sites. Fallback if no IG playlist is available: a small public YouTube playlist. YouTube doesn't need the cookies, but it confirms the playlist code path didn't regress from the cookies-prepend change.
   - Both should download successfully, confirming all three call-sites are wired correctly.

6. Tail logs after the smoke test to confirm no `level=ERROR` for these URLs:
   ```
   ssh sushe "sudo sushe-logs --since '5 minutes ago' --no-pager | rg -e 'level=ERROR|Successfully processed'"
   ```

**Cookie session expiry**: Instagram cookies typically last weeks-to-months. When the session expires, the bot will start failing with `login required` again. Refresh by re-exporting cookies from a logged-in browser session and repeating step 2 + restart in step 3 (only the restart from step 3, not the drop-in install which is one-time).

**Recommended hygiene**: use a dedicated Instagram account for the bot (not your personal one), to avoid risk of the main account being flagged for unusual access patterns.
