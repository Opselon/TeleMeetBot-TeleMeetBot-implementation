# syntax=docker/dockerfile:1.6
# ---- build stage -----------------------------------------------------------
FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build \
      -trimpath -ldflags "-s -w -X github.com/Opselon/TeleMeetBot/internal/config.Version=${VERSION}" \
      -o /out/telemeetbot ./cmd/telemeetbot

# ---- runtime stage ---------------------------------------------------------
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive \
    CHROME_BIN=/usr/bin/chromium \
    DISPLAY="" \
    GOMEMLIMIT=256MiB

# yt-dlp and ffmpeg power the optional Download & Stream feature. Both are
# optional at runtime: a missing binary degrades to Browser mode with an
# actionable message and never breaks the bot. Deno provides the JS runtime
# required by yt-dlp to solve YouTube JS challenges without format loss.
ARG TARGETARCH=amd64
RUN apt-get update && apt-get install -y --no-install-recommends \
      chromium \
      ca-certificates \
      tini \
      curl \
      unzip \
      ffmpeg \
      python3 \
      fonts-liberation \
      fonts-noto-core \
      fonts-noto-color-emoji \
      tzdata \
    && curl -fsSL https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp \
        -o /usr/local/bin/yt-dlp \
    && chmod 0755 /usr/local/bin/yt-dlp \
    && DENO_ARCH=$(if [ "$TARGETARCH" = "arm64" ]; then echo "aarch64"; else echo "x86_64"; fi) \
    && curl -fsSL "https://github.com/denoland/deno/releases/latest/download/deno-${DENO_ARCH}-unknown-linux-gnu.zip" \
        -o /tmp/deno.zip \
    && unzip /tmp/deno.zip -d /usr/local/bin/ \
    && chmod 0755 /usr/local/bin/deno \
    && rm -f /tmp/deno.zip \
    && rm -rf /var/lib/apt/lists/* \
    && fc-cache -f >/dev/null 2>&1 || true

COPY --from=build /out/telemeetbot /usr/local/bin/telemeetbot

RUN useradd --create-home --uid 10001 --shell /usr/sbin/nologin telemeet \
    && mkdir -p /data \
    && chown -R telemeet:telemeet /data

USER telemeet
WORKDIR /data
VOLUME ["/data"]
EXPOSE 18080

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl -fsS http://127.0.0.1:18080/livez || exit 1

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["telemeetbot", "--config", "/data/telemeetbot.yml"]
