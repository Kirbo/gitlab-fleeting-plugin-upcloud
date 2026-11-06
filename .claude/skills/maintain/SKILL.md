---
name: maintain
description: Maintenance pass for gitlab-fleeting-plugin-upcloud — land or refresh Renovate's pin bumps, keep mise.lock and go.mod in lockstep with mise.toml, run the full check suite (vet, golangci-lint, shellcheck, race tests, govulncheck), check the CI images, runner routing and the deploy tier, reconcile the docs, report, commit. Use when the user runs /maintain or asks for a maintenance/housekeeping pass on this repo.
---

# gitlab-fleeting-plugin-upcloud maintenance pass

**What this repo is:** a GitLab Runner fleeting plugin for UpCloud (Go, one binary per OS/arch,
released from `main` by `scripts/next-version.sh` + git-cliff), plus a small telemetry badge backend
(`telemetry/`) deployed over SSH by CI. Toolchain: `mise.toml` (exact pins, bumped by Renovate every
Monday) + the committed `mise.lock` (versions + sha256 for linux-arm64, linux-x64, macos-arm64).

Work from ground truth (registries, the lock, the live pipeline). Finish with a report and
logically scoped Conventional Commits — `fix(deps):` for anything that ends up in the binary (Go
toolchain, go.mod — it triggers a release), `ci:` / `ci(deps):` for CI-only, `docs:`, `chore:`.
**Never push without the owner's OK.**

## Phase 1 — Dependencies
1. Open Renovate merge requests: `glab mr list --label renovate` (or by author). Prefer landing
   them over hand-bumping; a bump Renovate has not proposed yet is fine by hand.
2. Every `mise.toml` pin moved → `mise lock` (set `MISE_GITHUB_TOKEN` if the GitHub API limit
   bites), commit `mise.lock` with it. CI's `mise.lock in sync` job fails otherwise, and the deploy
   jobs (locked mode) would refuse to run. Release-age rule: no tool version younger than 7 days.
3. Go toolchain: `mise.toml` `go` and `go.mod` (`go` / `toolchain`) move together (Renovate's
   "Go toolchain" group); `go mod tidy` leaves no diff.
4. `govulncheck` (`mise run vuln`, version from `[env] GOVULNCHECK_VERSION`) — bump that too.
5. `PSSCRIPTANALYZER_VERSION` moved → refresh `PSSCRIPTANALYZER_SHA256` beside it in
   `.gitlab-ci.yml` (the comment there has the commands). Renovate bumps only the version, so its
   MR fails the `psscriptanalyzer` job on the checksum until this is done.

## Phase 2 — Verify
`mise run check` (vet incl. the `integration` tag, golangci-lint, shellcheck, race tests, govulncheck)
must be clean — zero warnings. `mise run dist` builds every release target with checksums.
Never run `mise run test-integration` unasked: it creates real, billed UpCloud servers.

## Phase 3 — CI
- Images: the default `ghcr.io/jdx/mise:<version>-debian` and every other job image are exact tags
  (Renovate bumps them, `ci(deps):`); a floating `:latest` is a defect. The `psscriptanalyzer`
  image's tag floats across 7.5.x patches, so it also carries a `@sha256:` digest (Renovate
  refreshes it).
- ci-images: `test` (mise-cgo), `release` (glab-curl) and `.telemetry-deploy` (mise-deploy) run in
  the maintainer's own images (gitlab.com/KirboDev/agentic-coding/ci-images: tools baked in, so no
  job installs packages; rebuilt weekly for patches). Re-pin each to its newest build
  (`docker buildx imagetools inspect registry.gitlab.com/kirbodev/agentic-coding/ci-images/<name>:latest`,
  or the `Pin it as:` line of that project's latest `main` job). A job that needs a new tool gets
  it in ci-images, never an `apt-get` / `apk add` here.
- Runner routing (docs on the Mac Mini: `~/Documents/Claude/GitLab/BuildKit.md`): jobs use
  `tags: [$MAC_DOCKER]` — `macos-protected` on `main` (the workflow rule), `macos` otherwise — never
  the tag itself. Nothing depends on the `upcloud` runner (this plugin powers it).
- Deploy tier: `.telemetry-deploy` jobs restore no cache, install fnox + age `--locked`
  (`MISE_LOCKED=1`) and get `FNOX_AGE_KEY_FILE` only through the protected `release/deploy`
  environment (names only: `glab api "projects/KirboDev%2Fagentic-coding%2Fgitlab-fleeting-plugin-upcloud/variables" | jq -r '.[] | "\(.key) \(.environment_scope) protected=\(.protected)"'`).
- Last pipelines on `main` and the weekly schedule green:
  `glab api "projects/KirboDev%2Fagentic-coding%2Fgitlab-fleeting-plugin-upcloud/pipelines?ref=main&per_page=5" | jq -r '.[] | "\(.created_at) \(.source) \(.status)"'`.

## Phase 4 — Docs
`README.md` (requirements, install, development), `docs/install-*.md`, `docs/SECRETS.md` match the
code and the CI (tool versions, flags, the deploy tier's setup).

## Phase 5 — Report, then commit
Report per phase: bumps (old → new), lock refreshed, check output, CI health, doc fixes, held-back
items and why. Commit as above, each ending with the Co-Authored-By trailer. Do not push.
