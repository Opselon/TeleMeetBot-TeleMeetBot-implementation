package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ytdlpResult is what a yt-dlp fetch yields.
type ytdlpResult struct {
	Path        string
	ContentType string
	Bytes       int64
	Transcoded  bool
	Note        string
}

// maxExecRetries bounds a retry on a transient yt-dlp failure. One retry only:
// a persistent failure must surface as an error, not a spin.
const maxExecRetries = 1

// fetchYTDLP runs yt-dlp to fetch url into the managed directory.
//
// Security: yt-dlp receives exactly two vetted arguments — the output template
// (built from a server-generated id, never user text) and the validated URL.
// There is no shell anywhere in the chain, so no command injection surface.
func (m *Manager) fetchYTDLP(ctx context.Context, u, id string) (*ytdlpResult, error) {
	bin, ok := m.YTDLP()
	if !ok {
		return nil, &ErrMissingTool{
			Name: "yt-dlp",
			Path: firstNonEmpty(m.opts.YTDLPPath, "yt-dlp"),
			Hint: "install yt-dlp (https://github.com/yt-dlp/yt-dlp#installation) to enable Download & Stream for this source",
		}
	}
	// The output template is server-generated and id is alphanumeric; the
	// final name (with the container yt-dlp picked) is discovered afterwards
	// by listing the managed directory.
	if _, err := SafeJoin(m.opts.Dir, id+".mp4"); err != nil {
		return nil, fmt.Errorf("output path: %w", err)
	}

	args := []string{
		"--no-playlist", // one item, never a playlist avalanche
		"--no-warnings",
		"--no-progress",
		"--newline",
		"--no-check-certificate", // the SSRF gate already validated the host
	}
	// Datacenter egress is bot-blocked by YouTube, so the download must leave
	// through the same proxy Chromium uses. Omitting it is the difference
	// between "Sign in to confirm you're not a bot" and a working download.
	if p := strings.TrimSpace(m.opts.Proxy); p != "" {
		args = append(args, "--proxy", p)
	}
	args = append(args,
		"-f", m.formatSelector(), // best muxed mp4, else merged mp4/webm
		"-o", m.opts.Dir+string(os.PathSeparator)+id+".%(ext)s",
		"--print-json",
		"--encoding", "utf-8",
		u,
	)
	if m.opts.MaxBytes > 0 {
		// Bail out early on obviously oversized files.
		args = append([]string{"--match-filter", fmt.Sprintf(" filesize < %d", m.opts.MaxBytes)}, args...)
	}

	var res *ytdlpResult
	var last error
	for attempt := 0; attempt <= maxExecRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		stdout, stderr, err := m.runYTDLP(ctx, bin, args)
		if err == nil {
			res, err = m.collectYTDLP(ctx, id, stdout, stderr)
			if err == nil {
				return res, nil
			}
			last = err
			continue
		}
		// A missing dependency is not retryable.
		var missing *ErrMissingTool
		if errors.As(err, &missing) {
			return nil, err
		}
		// Carry yt-dlp's own diagnosis into the error: "exit status 1" alone
		// tells the operator nothing, while stderr names the real cause
		// (bot check, unsupported URL, missing format).
		last = fmt.Errorf("yt-dlp: %w%s", err, stderrSuffix(stderr))
		m.log.Warnf("[PIPELINE:download] %s: yt-dlp attempt %d failed: %s", id, attempt+1, truncateForLog(string(stderr), 200))
	}
	if last == nil {
		last = errors.New("yt-dlp produced no usable file")
	}
	return nil, last
}

// ytDLPJoinOK is the nil sentinel the output-path check compares against.
var ytDLPJoinOK error

// formatSelector asks yt-dlp for the best stream combination Chromium can play.
//
// Modern YouTube uploads are DASH-only: there is no progressive (muxed) format
// at all, so any selector that requires `acodec!=none` on a single stream fails
// with "Requested format is not available". The ladder therefore merges a
// video-only stream with an audio-only one first, and only then falls back to a
// progressive file for sources that still ship one:
//
//  1. h264 mp4 video + m4a audio — cheap to decode on a 2-vCPU host, where
//     AV1/VP9 1080p software decode would compete with Meet's encoder;
//  2. any mp4 video + m4a audio;
//  3. progressive mp4 with audio (older uploads, live captures);
//  4. any video + audio, letting ffmpeg merge whatever containers exist.
func (m *Manager) formatSelector() string {
	return strings.Join([]string{
		"bv*[ext=mp4][vcodec^=avc1]+ba[ext=m4a]",
		"b[ext=mp4][vcodec^=avc1][acodec!=none]",
		"bv*[ext=mp4]+ba[ext=m4a]",
		"b[ext=mp4][acodec!=none]",
		"bv*+ba/b",
	}, "/")
}

// collectYTDLP finds the file yt-dlp wrote (its name carries the real
// extension), verifies it, and optionally transcodes.
func (m *Manager) collectYTDLP(ctx context.Context, id string, stdout, stderr []byte) (*ytdlpResult, error) {
	path, err := m.findYTDLPFile(id, stdout)
	if err != nil {
		m.log.Warnf("[PIPELINE:download] %s: could not locate yt-dlp output: %v (stderr: %s)",
			id, err, truncateForLog(string(stderr), 200))
		return nil, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat yt-dlp file: %w", err)
	}
	if st.IsDir() {
		return nil, errors.New("yt-dlp target is a directory")
	}
	if st.Size() == 0 {
		_ = os.Remove(path)
		return nil, errors.New("yt-dlp produced an empty file")
	}
	if m.opts.MaxBytes > 0 && st.Size() > m.opts.MaxBytes {
		_ = os.Remove(path)
		return nil, fmt.Errorf("%w: yt-dlp wrote %s", errSizeLimit, humanBytes(st.Size()))
	}
	res := &ytdlpResult{Path: path, Bytes: st.Size(), ContentType: contentTypeForExt(path)}
	// Containers Chromium cannot decode (mkv/avi/ogv) get remuxed to mp4 when
	// ffmpeg is present; without ffmpeg the file is rejected with an
	// actionable error rather than played as a black frame.
	if needsTranscode(path) {
		if !m.opts.AllowTranscode {
			_ = os.Remove(path)
			return nil, fmt.Errorf("container %q needs transcoding but download.allow_transcode is off", extOf(path))
		}
		bin, ok := m.FFmpeg()
		if !ok {
			_ = os.Remove(path)
			return nil, &ErrMissingTool{
				Name: "ffmpeg",
				Path: firstNonEmpty(m.opts.FFmpegPath, "ffmpeg"),
				Hint: "install ffmpeg to remux incompatible containers (mkv/avi/ogv) to mp4",
			}
		}
		out, err := SafeJoin(m.opts.Dir, id+"-x.mp4")
		if err != nil {
			_ = os.Remove(path)
			return nil, fmt.Errorf("transcode output path: %w", err)
		}
		_ = os.Remove(out)
		if err := m.runFFmpeg(ctx, bin, path, out); err != nil {
			_ = os.Remove(path)
			_ = os.Remove(out)
			return nil, fmt.Errorf("ffmpeg transcode: %w", err)
		}
		// The remuxed file replaces the original.
		_ = os.Remove(path)
		final := out
		// Amend the manifest path/name to the transcoded file.
		res.Path = final
		res.Bytes = fileSizeOf(final)
		res.ContentType = "video/mp4"
		res.Transcoded = true
		res.Note = "remuxed to mp4 for Chromium compatibility"
	}
	return res, nil
}

// findYTDLPFile locates the file yt-dlp wrote for id: the template is
// id + ".%(ext)s", so any file with the stem is the output.
func (m *Manager) findYTDLPFile(id string, stdout []byte) (string, error) {
	ents, err := os.ReadDir(m.opts.Dir)
	if err != nil {
		return "", fmt.Errorf("read download dir: %w", err)
	}
	prefix := id + "."
	var found []os.DirEntry
	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if strings.HasPrefix(name, prefix) && !strings.HasPrefix(name, ".tmp-") {
			found = append(found, ent)
		}
	}
	if len(found) == 0 {
		return "", errors.New("no output file with the expected name")
	}
	// Prefer the newest when several extensions matched.
	newest := found[0]
	for _, ent := range found[1:] {
		if i, err := ent.Info(); err == nil {
			if j, err2 := newest.Info(); err2 == nil && i.ModTime().After(j.ModTime()) {
				newest = ent
			}
		}
	}
	path, err := SafeJoin(m.opts.Dir, newest.Name())
	if err != nil {
		return "", err
	}
	return path, nil
}

// runYTDLP executes the vetted argv (test-overridable).
func (m *Manager) runYTDLP(ctx context.Context, binary string, args []string) ([]byte, []byte, error) {
	if m.ytdlpFn != nil {
		return m.ytdlpFn(ctx, args)
	}
	return runExec(ctx, binary, args, m.opts.Timeout)
}

// runFFmpeg executes one transcode (test-overridable).
func (m *Manager) runFFmpeg(ctx context.Context, binary, in, out string) error {
	if m.ffmpegFn != nil {
		_, _, err := m.ffmpegFn(ctx, []string{"-y", "-i", in, "-c", "copy", out})
		return err
	}
	// "-c copy" remuxes without re-encoding: cheap, lossless, and usually
	// all that is needed to make a container Chromium understands.
	_, _, err := runExec(ctx, binary, []string{"-y", "-i", in, "-c", "copy", out}, m.opts.Timeout)
	return err
}

// runExec runs a command with a bounded timeout and captures both streams.
// No shell is involved: argv comes from vetted Go strings only.
func runExec(ctx context.Context, binary string, args []string, timeout time.Duration) ([]byte, []byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return []byte(stdout.String()), []byte(stderr.String()), normalizeExecError(err, ctx)
}

// normalizeExecError maps an exec failure to a descriptive error, distinguishing
// "binary missing" (actionable, Browser mode unaffected) from a normal failure.
func normalizeExecError(err error, ctx context.Context) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return &ErrMissingTool{Name: "binary", Path: "(exec)", Hint: err.Error()}
	}
	if ctx.Err() != nil {
		return fmt.Errorf("cancelled: %w", ctx.Err())
	}
	return err
}

// needsTranscode reports whether Chromium cannot decode the container without
// help. Chromium supports mp4 (h264/aac), webm (vp8/vp9/opus) natively.
func needsTranscode(path string) bool {
	switch strings.ToLower(extOf(path)) {
	case ".mkv", ".avi", ".ogv", ".oga", ".flac", ".wav", ".aac", ".m4a":
		return true
	default:
		return false
	}
}

// extOf is the lower-cased extension of path.
func extOf(path string) string {
	i := strings.LastIndexByte(path, '.')
	if i < 0 {
		return ""
	}
	return strings.ToLower(path[i:])
}

// contentTypeForExt maps an extension to a MIME type for the loopback player.
func contentTypeForExt(path string) string {
	switch strings.ToLower(extOf(path)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mkv":
		return "video/x-matroska"
	case ".mov":
		return "video/quicktime"
	case ".ogv":
		return "video/ogg"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".aac":
		return "audio/aac"
	case ".flac":
		return "audio/flac"
	case ".wav":
		return "audio/wav"
	case ".ogg":
		return "audio/ogg"
	case ".opus":
		return "audio/opus"
	default:
		return "application/octet-stream"
	}
}

// fileSizeOf is nil-safe stat.
func fileSizeOf(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// stderrSuffix renders the last meaningful stderr line as an error suffix, so
// the caller sees yt-dlp's reason instead of a bare exit status. Empty when
// stderr carries nothing usable.
func stderrSuffix(stderr []byte) string {
	lines := strings.Split(strings.TrimSpace(string(stderr)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		return ": " + truncateForLog(line, 300)
	}
	return ""
}

// isBinaryMissing recognises the shell's "not found" report on stderr. yt-dlp
// failing on a bad URL is an ordinary error, not a missing tool.
func isBinaryMissing(stderr []byte) bool {
	t := strings.ToLower(string(stderr))
	return strings.Contains(t, "not found") || strings.Contains(t, "no such file or directory")
}
