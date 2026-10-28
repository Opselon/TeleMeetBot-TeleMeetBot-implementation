# TeleMeetBot v1.5.0 Release Notes

TeleMeetBot v1.5.0 is a major release transforming TeleMeetBot into a genuinely easy-to-install, production-ready application across Windows, Linux, macOS, and Docker. It introduces a unified setup wizard, cross-platform dependency management, native Windows setup executables, live media download progress tracking, and robust service lifecycle automation.

## Highlights

### 1. Canonical 9-Step Unified Setup Wizard
- **One Source of Truth**: Unified setup implementation across all platforms accessible via `telemeetbot setup`, `telemeetbot --wizard`, or launching `TeleMeetBot-Setup.exe`.
- **Environment & Privileges Detection**: Automatically inspects OS, architecture, user privileges (elevated/root vs user-space), and available service supervisors (systemd, Windows SCM, launchd).
- **Installation Profiles**:
  - **Full Installation**: Configures core Chromium WebRTC streaming plus high-performance Download & Stream media tools (yt-dlp, FFmpeg, Deno).
  - **Minimal Installation**: Core Chromium WebRTC streaming only for direct browser-based presentation.
- **Fail-Safe Configuration**: Automatically creates `.bak` backups before modifying existing configs with zero-downtime `--restore-backup` rollback.
- **Secrets Hardening**: Telegram Bot API tokens are masked across logs, summaries, and diagnostic reports.

### 2. Cross-Platform Dependency Manager
- **Automated Detection & Installation**: Discovers and inspects Chromium, yt-dlp, FFmpeg, and Deno across system paths and configuration overrides.
- **Native Package Manager Integration**:
  - **Linux**: Automatic package detection and installation for `apt-get`, `dnf`, `pacman`, `apk`, and `zypper`.
  - **Windows**: Package installation via `winget` with actionable fallback guidance for missing environments.
  - **macOS**: Integration with Homebrew (`brew`) with architecture support for both Apple Silicon (ARM64) and Intel (AMD64).
- **Graceful Degradation**: Clearly differentiates critical missing browser components from optional media extractors.

### 3. Native Windows Installer & Fixed PowerShell Script
- **Native Setup Executable**: `TeleMeetBot-Setup-windows-amd64.exe` and `TeleMeetBot-Setup-windows-arm64.exe` allow Windows operators to install and configure TeleMeetBot via an interactive wizard with single-click execution.
- **Transactional Installation**: Binaries are staged, validated, and installed atomically with support for standard user directories (`%LOCALAPPDATA%\Programs\TeleMeetBot`) when run without administrator rights.
- **Repaired `install.ps1`**: Fixed variable scoping in download URL construction, fixed unprivileged config paths, and ensured configuration completes before Windows service registration.

### 4. POSIX Shell Installer & Service Lifecycle
- **Idempotent Installation (`install.sh`)**: Hardened POSIX-compliant installation script with SHA-256 verification and atomic service startup.
- **Full CLI Subcommands**: Native CLI commands for `telemeetbot setup`, `telemeetbot doctor`, `telemeetbot uninstall`, and `telemeetbot version`.
- **Diagnostic Engine (`telemeetbot doctor`)**: Verifies configuration, Telegram token authentication, Chromium launch, media pipeline tools, disk space, and network health.

### 5. Media Engine & UI Enhancements (PR #42 & PR #43)
- **Live Download Progress**: Real-time progress bars and transfer speeds (KB/s, MB/s) displayed directly on the Telegram session card during media preparation.
- **WebRTC Bandwidth Boost**: Optimized WebRTC transmission flags (`contentHint = "motion"`) and Chromium rendering parameters for ultra-smooth stream playback.
- **Subview Retention**: Interactive subviews (Playback Speed, Video Quality, Captions, Media Queue) persist reliably across periodic card refreshes.
