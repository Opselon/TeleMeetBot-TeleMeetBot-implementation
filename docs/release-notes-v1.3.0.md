# TeleMeetBot v1.3.0 — the one-command install actually works

**A patch release with one headline fix: the published one-command installer was
broken on every Debian, Ubuntu and Alpine host.** Everything else is hardening
that makes that class of failure impossible to ship again.

## The bug

The v1.2.0 install script used bash-only syntax (`[[ ]]`, `${var//x/y}`), but
the documented one-command install pipes it straight into `sh`:

```bash
curl -fsSL https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/latest/download/install.sh | sh
```

On Debian and Ubuntu `/bin/sh` is **dash**; on Alpine it is **busybox**. Neither
implements `[[`, so the script died before installing anything:

```
sh: 36: [[: not found
sh: 80: [[: not found
error: resolved version 'latest' does not look like a tag (vX.Y.Z); refusing to install
```

The failure was silent in two ways: the process usually exited 0 (so `curl | sh`
looked successful to a caller not watching stderr), and CI never caught it
because every CI step invoked the script with `bash` — a different interpreter
than the one the README tells users to use.

## The fix

`scripts/install.sh` is rewritten in POSIX `sh` and carries a `shellcheck -s sh`
gate. Verified under **dash**, **busybox** and **bash**, and in clean
minimal-container environments (Debian `bookworm-slim` and Alpine) with no
build toolchain installed.

Nothing about the flag surface or the install layout changed:

```bash
curl -fsSL https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/latest/download/install.sh | sh
```

## Hardening in this release

- **Post-install version verification.** The installer now runs the binary it
  just installed and matches its `--version` against the release tag. A corrupt
  download, a truncated archive, or a version-stamping mistake in the build
  aborts the install instead of being reported as success.
- **CI now tests the published invocation.** The installer smoke job pipes the
  script through `sh` (with `curl | sh`, exactly as documented) instead of
  running it under `bash`, and asserts `dash -n` plus `shellcheck -s sh`.
- **Version-mismatch regression test.** A new CI case serves an archive whose
  binary reports a different version than the requested tag and requires the
  installer to refuse it.
- **Release smokeJobs assert the stamped version.** Every release binary's
  `--version` must contain the release tag, so the installer's new version gate
  can never be defeated by broken `ldflags` wiring.
- **Post-publish install verification.** The release workflow now runs the
  published installer against the release it just published — downloading the
  real `install.sh` from the release assets and installing into a sandbox — so
  a release cannot be announced as working without a user-side install
  succeeding.
- **Documentation fixes.** The website install guides pointed at the stale
  `raw.githubusercontent.com/…/main/scripts/install.sh` bootstrap URL (an
  unpinned, branch-following URL that bypasses checksum verification); they now
  use the release asset. The README pin examples were updated from v1.1.0 to
  v1.2.0.

## Upgrading from v1.2.0

```bash
# re-run the one-command install against the new release
curl -fsSL https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/download/v1.3.0/install.sh | sh -s -- --version v1.3.0
```

Or, if you installed with a custom prefix:

```bash
curl -fsSL https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/download/v1.3.0/install.sh \
  | sh -s -- --version v1.3.0 --prefix /your/prefix --config-dir /your/config
```

The install is idempotent: re-running over an existing install replaces the
binary and leaves the config, data directory and service registration
untouched. No configuration key was removed or renamed; every v1.2.0 config
loads unchanged.

If you installed v1.0.0/v1.1.0 with the old `raw.githubusercontent.com`
bootstrapping URL, switch to the pinned release URL above — the branch URL
serves whatever is on `main` today and validates nothing.

## Artifacts

Native binaries for Linux (amd64, arm64, arm v6/v7, 386), macOS (Intel, Apple
Silicon) and Windows (amd64, arm64), plus `SHA256SUMS` and the installers
themselves:

```bash
curl -fsSLO https://github.com/Opselon/TeleMeetBot-TeleMeetBot-implementation/releases/download/v1.3.0/SHA256SUMS
sha256sum -c SHA256SUMS --ignore-missing
```

Docker image published as
`ghcr.io/opselon/telemeetbot-telemeetbot-implementation:v1.3.0` (and `:latest`),
multi-arch for `linux/amd64` and `linux/arm64`.
