#!/usr/bin/env bash
# TeleMeetBot release helper.
#
#   ./scripts/release.sh build    cross-compile dist/ archives (no network writes)
#   ./scripts/release.sh publish  build, then tag + push + create the GitHub release
#
# Requires: go, git, sha256sum, tar, gh (only for publish).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

REPO="${REPO:-Opselon/TeleMeetBot-TeleMeetBot-implementation}"
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo v0.0.0-dev)}"
DIST="$ROOT/dist"
TARGETS=("linux/amd64" "linux/arm64" "linux/arm/v6" "linux/arm/v7" "linux/386")

log() { printf '\033[36m==>\033[0m %s\n' "$*"; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

build() {
  command -v go >/dev/null || die "go is not installed"
  rm -rf "$DIST"
  mkdir -p "$DIST"

  for target in "${TARGETS[@]}"; do
    os="${target%%/*}"
    arch="${target#*/}"
    goarch="$arch"
    goarm=""
    if [[ "$arch" == arm/* ]]; then
      goarch="arm"
      goarm="${arch#arm/}"
    fi
    out="telemeetbot-${os}-${arch//\//}"
    log "building $out ($VERSION)"

    stage="$(mktemp -d)"
    CGO_ENABLED=0 GOOS="$os" GOARCH="$goarch" GOARM="${goarm:-}" \
      go build -trimpath -ldflags "-s -w -X github.com/Opselon/TeleMeetBot/internal/config.Version=${VERSION}" \
      -o "$stage/telemeetbot" ./cmd/telemeetbot

    cp README.md README.fa.md LICENSE "$stage"/ 2>/dev/null || true
    cp -r deploy "$stage"/
    cp scripts/install.sh "$stage"/
    tar -C "$stage" -czf "$DIST/${out}.tar.gz" .
    rm -rf "$stage"
  done

  ( cd "$DIST" && sha256sum telemeetbot-linux-*.tar.gz > SHA256SUMS )
  log "artifacts:"
  ls -lh "$DIST"
}

publish() {
  command -v gh >/dev/null || die "gh CLI is required to publish"
  [[ "$VERSION" == v* ]] || die "VERSION must look like v1.2.3 (got '$VERSION')"
  build

  if git rev-parse -q --verify "refs/tags/$VERSION" >/dev/null; then
    log "tag $VERSION already exists locally"
  else
    git tag -a "$VERSION" -m "TeleMeetBot $VERSION"
  fi
  git push origin HEAD
  git push origin "$VERSION"

  log "creating GitHub release $VERSION"
  gh release create "$VERSION" \
    --repo "$REPO" \
    --title "TeleMeetBot $VERSION" \
    --notes-file "$ROOT/docs/release-notes-$VERSION.md" \
    "$DIST"/*.tar.gz "$DIST"/SHA256SUMS

  log "release published: https://github.com/$REPO/releases/tag/$VERSION"
}

case "${1:-build}" in
  build) build ;;
  publish) publish ;;
  *) die "usage: $0 [build|publish]" ;;
esac
