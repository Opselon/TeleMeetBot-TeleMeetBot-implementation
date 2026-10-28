# Dependency update policy — TeleMeetBot

This document is the authoritative policy the scheduled dependency automation in
this repository follows. It exists so that `go get -u` never surprises anyone.

## Reach of the audit

TeleMeetBot is a Go application with a Node.js (Astro) docs site, packaged in
Docker and running on GitHub Actions. Every dependency surface found in the
repository is covered:

| Ecosystem | Manifest(s) | Update engine | Schedule |
|---|---|---|---|
| Go modules | `go.mod`, `go.sum` | Dependabot (`gomod`) + `dependency-audit.yml` | weekly |
| Node (docs site) | `website/package.json`, `website/package-lock.json` | Dependabot (`npm`) + audit workflow | weekly |
| Docker base images | `Dockerfile`, `docker-compose.yml` | manual, surfaced by `runtime-pins` audit job | weekly |
| GitHub Actions | `.github/workflows/*.yml` | Dependabot (`github-actions`) | weekly |
| Runtime pins (Go toolchain, Node, vendored yt-dlp / deno) | workflow env, `Dockerfile` | manual, surfaced by `runtime-pins` audit job | weekly |

No Python, Rust, CocoaPods, Gradle, Maven, Ruby or Perl dependency manifests
exist in this repository — the audit confirmed it, it does not need covering.

## Update classification

- **security** — there is a published advisory (GHSA) affecting a version we
  use, and the fix is compatible. These are Dependabot's highest priority and
  are reviewed first, regardless of the weekly grouping.
- **patch / minor** — backwards-compatible. Grouped per ecosystem into a single
  weekly PR (`deps(go)`, `deps(website)`, `deps(ci)`, `deps(docker)`) so a
  human reviews one PR instead of fifteen.
- **major** — always isolated into its own PR, never grouped. A major bump is
  treated as *blocking* until the validation pipeline in §1.3 passes and the
  reviewer confirms no application changes are needed.

Risky majors are explicitly **not** auto-taken:

| Package | Current | Latest | Decision |
|---|---|---|---|
| `astro` | 5.18.2 | 7.4.3 | **Held — verified breaking.** See "astro 5 → 7" below. |
| `@astrojs/mdx` | 4.3.14 | 8.1.2 | Held — moves with the astro major. |
| `github.com/go-rod/rod` | 0.116.2 | 0.116.2 | At latest. |
| `github.com/ysmood/fetchup` | 0.2.3 | 0.5.3 | **Pinned to ≤0.2.3 due to a genuine API break**: `fetchup.New()` signature changed from `New(dir string, urls ...string)` to `New(urls ...string)` in 0.5.x. rod's launcher depends on the old signature, so anything above 0.2.3 breaks `go build ./...`. Do not bump without a rod release that adapts to it. |
| `github.com/ysmood/got` | 0.40.0 | 0.41.0 | Downgraded from 0.41.0 as the companion pin for the `fetchup` constraint above. |
| `@playwright/test` | 1.64.1 | 1.64.1 | At latest compatible with `^1.55.0`. |

### astro 5 → 7 (verified, deliberately held)

The initial audit found 6 npm advisories against the website tree — most
consequentially **GHSA-26w7-cxv4-gfx2 (astro AVIF image-optimization RCE,
CVSS 9.8)** and GHSA-2pvr-wf23-7pc7 (SSRF in prerendered error pages, CVSS
7.5). Every one of them is only patched in astro ≥ 6.x/7.x, so `npm audit`
on the 5.x tree has no in-range fix.

Upgrading was tried and **does not work today**:

1. astro 7 replaced the default Markdown processor with Sätteri; the build
   refuses to run until `@astrojs/markdown-remark` is installed as a new
   dependency and the `markdown.rehypePlugins`/`remarkPlugins` config is
   migrated onto it.
2. After adding that dependency, `astro build` dies inside
   `lightningcss minify` with `[lightningcss minify] Unexpected token
   SquareBracketBlock` on this site's CSS — a Vite/lightningcss version
   incompatibility carried by astro 7.

Exposure on this site is low (verified, not assumed): `define:vars` is not
used anywhere in `src/`, no `astro:assets` `<Image>`/`<Picture>` component is
used (so no AVIF image-optimization path runs in this site's build), and the
site is statically prerendered. The `define:vars` XSS advisory nonetheless
applies because `BaseHead.astro` uses `<script is:inline>`, which is the
sink class the advisory describes.

**Decision:** the astro major stays held and is tracked as the single highest
priority manual dependency migration. Dependabot is configured to ignore
`version-update:semver-major` for `astro` and `@astrojs/mdx` so it does not
re-open this every week; the audit workflow's npm-audit job keeps reporting
the advisories so they are not forgotten. Taking the upgrade means: install
`@astrojs/markdown-remark`, migrate `astro.config.mjs` markdown plugins,
resolve the lightningcss minify failure (or set `build.inlineStylesheets` /
disable the minifier), then run the full website build + SEO + contrast
suites.

## Constraints discovered during the initial audit (important)

These are the *load-bearing* dependency relationships in this codebase. Any
future update PR must respect them:

1. **`fetchup` ≤ 0.2.3 is load-bearing.** `internal/setup/browser_install.go`
   downloads a Chromium build via `fetchup`. Upgrading `fetchup` past 0.2.3
   changes the `New()` signature and breaks compilation of
   `github.com/go-rod/rod/lib/launcher`, which is a direct dependency of the
   browser automation stack. Verified by `go build ./...` failing on 0.5.3 and
   succeeding on 0.2.3.
2. **`golang.org/x/sys` and `modernc.org/sqlite` are *direct* requires, not
   indirect.** `internal/setup/browser_install.go` and `internal/svcinst`
   import `golang.org/x/sys` directly; `modernc.org/sqlite` is used directly by
   the storage layer. `go mod tidy` correctly promotes them from the indirect
   block. A future diff that moves them back to `// indirect` is wrong.
3. **Go minor bumps of the module set are safe and verified**: the initial
   update run (libc 1.79.0 → 1.79.1, and the promotion of the two direct
   modules above) passed `go build ./...` and all 13 packages of
   `go test ./... -count=1`.
4. **`aiogram/telegram-bot-api:latest`** is intentionally `:latest` for the
   vendored local Bot API server in the Dockerfile; its update is a manual,
   reviewed decision, not a scheduled bump.
5. **Runtime pins are deliberate:** `golang:1.26-bookworm` (build stage),
   `debian:bookworm-slim` (runtime stage), Node 22 for the site. Dependabot's
   `docker` ecosystem watches these tags; the audit workflow reports drift.

## Nightly vs weekly split

| Run | When (UTC) | What it does | PRs? |
|---| Dependabot daily security PRs |
| nightly audit | 03:00, Mon–Sat | `go list -m -u`, govulncheck, npm audit, lockfile consistency. No Docker, no site build. | no |
| weekly audit | 03:00, Sunday | everything nightly has, plus Docker/runtime pin report + full site build smoke. | no |
| Dependabot gomod+npm+actions+docker | Mon 06:00–07:30 UTC | opens grouped PRs for compatible updates; isolated PRs for security + majors. | yes |

The slots are deliberately offset (audit at 03:00 Sunday/Mon–Sat, Dependabot on
Monday morning) so the two never compete for runners and never duplicate work.
The audit workflow **never** opens a PR: it is detection and reporting only.

## PR policy

- PRs are created **only when there is a real diff** — a scheduled run that
  finds no updates completes successfully without creating an empty PR.
- Branches: `dependabot/go_modules/…`, `dependabot/npm/website/…`, etc.
- CI must be green on the PR branch before a human reviews it. Never merge a
  PR solely because automation is green.
- Automated recovery is capped (see §"Failure recovery" below); a PR that
  cannot be fixed safely is left for a human with an exact failure report.
- Every PR contains the dependency table, update type, release-note links and
  the validation commands actually run (§1.3 of the main policy doc).

## Failure recovery

The automation is deliberately conservative:

- Dependabot auto-rebases its PR against `main` (rebase-strategy: auto), so a
  PR whose only problem is drift gets a fresh CI run automatically.
- When a check fails, the failure must be diagnosed from **actual logs** before
  any fix. If the root cause is the dependency update itself and the fix is a
  one-line, testable correction, it is applied; otherwise the dependency is
  pinned back to the last verified version (as done for `fetchup` in the
  initial run).
- **Infinite loops are forbidden.** No more than 3 automatic correction
  attempts per PR; after that the PR is annotated with the exact failure and
  left open for a human. Identical failing pushes are never repeated.
- Tests, security checks and branch protection are never weakened to make CI
  green.

## What is never done

- No credential, token or generated secret is ever committed or included in a
  PR description.
- No push to `main`; no auto-merge. Merge decisions stay with a human.
- No dependency is silently removed and no production runtime requirement is
  silently changed.
- The application's own version (`internal/version` / release tags) is never
  bumped because a dependency moved.
