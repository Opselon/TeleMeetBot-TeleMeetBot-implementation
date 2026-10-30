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
	// Selected is the format yt-dlp reports it chose, parsed from the run's
	// own --print output. Zero value = not reported.
	Selected SelectedFormat
}

// maxExecRetries bounds retries on a transient yt-dlp failure.
const maxExecRetries = 2

// proxyBlocked reports whether a yt-dlp failure is an egress block rather
// than a genuine "this URL cannot be downloaded" verdict. These are the two
// shapes YouTube uses to wall off a single datacenter egress at the API
// layer:
//
//   - "Unable to download API page: N bytes read (caused by <IncompleteRead>)"
//   - YouTube truncates the innertube response mid-body, which is a
//     per-IP rate limit, not a property of the video.
//   - "Sign in to confirm you're not a bot" - the datacenter IP itself is
//     flagged.
//
// Neither says anything about the requested media, so both are worth a
// different route. Everything else (unsupported URL, missing format,
// age-restricted content) is a real verdict and must NOT burn a fallback.
func proxyBlocked(stderr []byte) bool {
	t := strings.ToLower(string(stderr))
	// Covers the bare http.client marker ("IncompleteRead(...)") and yt-dlp's
	// wrapped form ("<IncompleteRead: N bytes read>"). Matching the short
	// stem keeps this from drifting with yt-dlp's phrasing.
	if strings.Contains(t, "incomplet") {
		return true
	}
	return strings.Contains(t, "sign in to confirm you're not a bot") ||
		strings.Contains(t, "sign in to confirm you’re not a bot") ||
		strings.Contains(t, "http error 429")
}

// stderrOf pulls the embedded yt-dlp stderr text out of a wrapped fetch
// error (the %w in "yt-dlp: exit status 1: ERROR: ..."). The ladder decision
// reads the real diagnosis, not a bare "exit status 1".
func stderrOf(err error) []byte {
	if err == nil {
		return nil
	}
	return []byte(err.Error())
}

// transientFailure reports whether a subprocess failure looks like a
// short-lived transport problem (connection reset, broken pipe, TLS EOF)
// rather than a completed run that returned a verdict. The diagnosis lives in
// yt-dlp's stderr, so both the error text and the stderr are inspected. The
// historical behaviour retried these, and the retry counters the logs rely on
// come from that contract.
func transientFailure(err error, stderr []byte) bool {
	t := strings.ToLower(string(stderr))
	if err != nil {
		t += " " + strings.ToLower(err.Error())
	}
	return strings.Contains(t, "connection reset") ||
		strings.Contains(t, "broken pipe") ||
		strings.Contains(t, "eof") ||
		strings.Contains(t, "no such file or directory")
}

// egresses returns the ordered list of egresses a YouTube download may use:
// the configured primary proxy first, then each fallback. The empty string
// means "direct connection" and is only present when the operator configured
// no proxy at all.
func (m *Manager) egresses() []string {
	var out []string
	if p := strings.TrimSpace(m.opts.Proxy); p != "" {
		out = append(out, p)
	}
	for _, p := range m.opts.FallbackProxies {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		out = append(out, "")
	}
	return out
}

// ytdlpArgs builds the vetted argv for one yt-dlp run. proxy is the egress
// that run must use; the empty string means a direct connection (no --proxy
// flag at all, matching the historical behaviour).
func (m *Manager) ytdlpArgs(u, id, quality, subLang, proxy string) []string {
	args := []string{
		"--no-playlist", // one item, never a playlist avalanche
		"--no-warnings",
		"--newline",
		"--no-check-certificate", // the SSRF gate already validated the host
		// Network resilience. The proxied egress (WARP/free-pool xray inbound
		// on 172.17.0.1:3339) periodically tears sockets down mid-handshake:
		// the extractor then reports "Unable to download API page:
		// TLS/SSL connection has been closed (EOF)" and, without retries below,
		// the whole session dies on a single dropped TLS handshake.
		"--retries", "10", // -R: HTTP/network attempts, covers TLS EOF resets
		"--extractor-retries", "3",
		"--file-access-retries", "3",
		// Typed sleeps use the TYPE:EXPR form (colon, not equals): yt-dlp
		// rejects "file_access=1" with "invalid http retry sleep expression".
		"--retry-sleep", "http:1:4", // bounded backoff between network retries
		"--retry-sleep", "extractor:1",
		"--retry-sleep", "file_access:1",
		"--socket-timeout", "30", // a hung socket must not pin the download
		"--force-ipv4", // the WARP egress has no working IPv6 route; AAAA attempts fail 100%
		// Maximize download throughput across network interface:
		// Multi-threaded fragment fetching + optimized HTTP chunk and buffer sizes.
		"--concurrent-fragments", "8",
		"--buffer-size", "1024K",
		"--http-chunk-size", "10M",
		// Real-time progress output for live progress reporting:
		"--progress",
		"--progress-delta", "0.5",
		"--progress-template", "download:%(progress.downloaded_bytes)s/%(progress.total_bytes)s/%(progress.total_bytes_estimate)s/%(progress.speed)s",
	}
	// Datacenter egress is bot-blocked by YouTube, so the download must leave
	// through the same proxy Chromium uses. Omitting it is the difference
	// between "Sign in to confirm you're not a bot" and a working download.
	if p := strings.TrimSpace(proxy); p != "" {
		args = append(args, "--proxy", p)
	}
	// ffmpeg decides whether the merge ladder is usable at all: asking
	// yt-dlp for bv*+ba without it just fails the download.
	selector := FormatSelectorFor(quality, m.FFmpegOK())
	args = append(args,
		"-f", selector, // policy ladder: best format at or below the cap
		"--merge-output-format", "mp4", // pin the container ffmpeg produces
		"-o", m.opts.Dir+string(os.PathSeparator)+id+".%(ext)s",
		// NEVER add --print-json: it implies --simulate, so yt-dlp prints
		// metadata and writes no file. That is exactly what made every
		// Download & Stream session die at ~3s with "no output file with the
		// expected name" and an empty stderr.
		"--no-simulate",
	)
	if lang := strings.ToLower(strings.TrimSpace(subLang)); lang != "" && lang != "off" {
		// Ride the caption track along with the video: the manual track when
		// the source ships one, automatic otherwise. The artifact is
		// verified and converted after the fetch (prepareSubtitle).
		args = append(args,
			"--write-subs",
			"--write-auto-subs",
			"--sub-langs", lang,
			"--sub-format", "vtt/srt/best",
		)
	}
	if m.opts.MaxBytes > 0 {
		// Abort an oversized download while it runs. Never use --match-filter
		// here: a DASH-merged selection has no exact `filesize` (only
		// filesize_approx), so the filter rejected every format and yt-dlp
		// exited 0 having written nothing.
		args = append(args, "--max-filesize", fmt.Sprintf("%d", m.opts.MaxBytes))
	}
	args = append(args,
		// Report what was actually selected. With --no-simulate, --print
		// WHEN:TEMPLATE prints during the real download; after_move runs
		// once per output file. The comma-separated fields are parsed in
		// collectYTDLP and end up on the session's quality info. Commas (not
		// pipes) keep the argv free of shell metacharacters.
		"--print", "after_move:%(format_id)s,%(width)s,%(height)s,%(fps)s,%(vcodec)s,%(acodec)s,%(ext)s,%(tbr)s",
		// The URL is the positional argument and must come last: an argument
		// that follows it is treated as a second video.
		u,
	)
	return args
}

// fetchYTDLP runs yt-dlp to fetch url into the managed directory.
//
// Security: yt-dlp receives exactly two vetted arguments — the output template
// (built from a server-generated id, never user text) and the validated URL.
// There is no shell anywhere in the chain, so no command injection surface.
func (m *Manager) fetchYTDLP(ctx context.Context, u, id, quality, subLang string) (*ytdlpResult, error) {
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

	onProgress := func(line string) {
		if !strings.HasPrefix(line, "download:") {
			return
		}
		parts := strings.Split(strings.TrimPrefix(line, "download:"), "/")
		if len(parts) >= 4 {
			var downloaded, total, speed int64
			_, _ = fmt.Sscanf(parts[0], "%d", &downloaded)
			if _, err := fmt.Sscanf(parts[1], "%d", &total); err != nil || total <= 0 {
				_, _ = fmt.Sscanf(parts[2], "%d", &total)
			}
			_, _ = fmt.Sscanf(parts[3], "%d", &speed)
			if downloaded > 0 {
				m.reportProgress(Progress{
					URL: u, ID: id, Bytes: downloaded, Total: total, Speed: speed,
					Stage: StageDownload, Kind: "yt-dlp",
					Percent: percentOf(downloaded, total),
					ETA:     etaOf(downloaded, total, speed),
				})
			}
		}
	}

	// Egress ladder: the primary proxy first, then each configured fallback.
	// YouTube rate-limits a single datacenter egress at the API layer (the
	// extractor then dies with "Unable to download API page: N bytes read
	// (caused by <IncompleteRead>)") even though the video itself is
	// perfectly downloadable over a different route, so a blocked egress is
	// retried over the next one instead of failing the whole session. Only
	// egress-shaped failures advance the ladder; a genuine verdict such as
	// "unsupported URL" or "Requested format is not available" fails fast on
	// the first route. stderr carries the last run's diagnosis so the ladder
	// decision below sees the real failure text.
	routes := m.egresses()
	var res *ytdlpResult
	var last error
	var stderr []byte
	for ri, proxy := range routes {
		args := m.ytdlpArgs(u, id, quality, subLang, proxy)
		label := proxy
		if label == "" {
			label = "direct"
		}
		for attempt := 0; attempt <= maxExecRetries; attempt++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if attempt > 0 {
				d := time.Duration(attempt) * time.Second
				if m.retryDelay != nil {
					d = m.retryDelay(attempt)
				}
				if d > 0 {
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(d):
					}
				}
			}
			stdout, runStderr, rerr := m.runYTDLP(ctx, bin, args, onProgress)
			if rerr == nil {
				res, rerr = m.collectYTDLP(ctx, id, stdout, runStderr)
				if rerr == nil {
					if ri > 0 {
						m.log.Infof("[PIPELINE:download] %s: extracted via fallback egress %s (primary %s was blocked)", id, label, primaryLabel(m.opts.Proxy))
					}
					return res, nil
				}
				last = rerr
				continue
			}
			// A missing dependency is not retryable.
			var missing *ErrMissingTool
			if errors.As(rerr, &missing) {
				return nil, rerr
			}
			// Keep the last diagnosis for the egress-ladder decision below:
			// proxyBlocked() reads exactly this text.
			stderr = runStderr
			if len(stderr) == 0 {
				stderr = stderrOf(rerr)
			}
			// Carry yt-dlp's own diagnosis into the error: "exit status 1" alone
			// tells the operator nothing, while stderr names the real cause
			// (bot check, unsupported URL, missing format).
			last = fmt.Errorf("yt-dlp: %w%s", rerr, stderrSuffix(stderr))
			m.log.Warnf("[PIPELINE:download] %s: yt-dlp attempt %d via %s failed: %s", id, attempt+1, label, truncateForLog(string(stderr), 200))
			// A deterministic extractor verdict ("Requested format is not
			// available", "Unsupported URL") does not change between attempts:
			// stop rerunning identical argv and let the egress ladder decide
			// whether another route could help. Transient, rate-limit-shaped
			// failures keep the historical bounded retries to ride out a flap.
			if !proxyBlocked(stderr) && !transientFailure(rerr, stderr) {
				break
			}
		}
		if ri+1 < len(routes) && proxyBlocked(stderr) {
			m.log.Warnf("[PIPELINE:download] %s: egress %s blocked by the source (%s); trying next egress %s", id, label, truncateForLog(strings.TrimSpace(string(stderr)), 120), routes[ri+1])
			continue
		}
		// A real verdict (or a route error that is not egress-shaped): the
		// remaining fallbacks cannot help either.
		break
	}
	if last == nil {
		last = errors.New("yt-dlp produced no usable file")
	}
	return nil, last
}

// primaryLabel renders the primary egress for a log line ("direct" when none
// is configured).
func primaryLabel(p string) string {
	if p = strings.TrimSpace(p); p != "" {
		return p
	}
	return "direct"
}

// ytDLPJoinOK is the nil sentinel the output-path check compares against.
var ytDLPJoinOK error

// FFmpegOK reports whether a usable ffmpeg binary is configured: the merge
// ladders are only valid when a merge can actually happen.
func (m *Manager) FFmpegOK() bool {
	_, ok := m.FFmpeg()
	return ok
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
	res.Selected = parseSelectedFromPrint(stdout)
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

// lineWriter splits incoming process output into lines and intercepts download progress.
type lineWriter struct {
	buf     strings.Builder
	onLine  func(string)
	lineBuf []byte
}

func (lw *lineWriter) Write(p []byte) (n int, err error) {
	for _, b := range p {
		if b == '\n' || b == '\r' {
			if len(lw.lineBuf) > 0 {
				line := string(lw.lineBuf)
				lw.lineBuf = lw.lineBuf[:0]
				if lw.onLine != nil {
					lw.onLine(line)
				}
				if !strings.HasPrefix(line, "download:") {
					lw.buf.WriteString(line)
					lw.buf.WriteByte('\n')
				}
			}
		} else {
			lw.lineBuf = append(lw.lineBuf, b)
		}
	}
	return len(p), nil
}

func (lw *lineWriter) flush() {
	if len(lw.lineBuf) > 0 {
		line := string(lw.lineBuf)
		lw.lineBuf = lw.lineBuf[:0]
		if lw.onLine != nil {
			lw.onLine(line)
		}
		if !strings.HasPrefix(line, "download:") {
			lw.buf.WriteString(line)
			lw.buf.WriteByte('\n')
		}
	}
}

// runYTDLP executes the vetted argv (test-overridable).
func (m *Manager) runYTDLP(ctx context.Context, binary string, args []string, onLine func(string)) ([]byte, []byte, error) {
	if m.ytdlpFn != nil {
		return m.ytdlpFn(ctx, args)
	}
	return runExec(ctx, binary, args, m.opts.Timeout, onLine)
}

// runFFmpeg executes one transcode (test-overridable).
func (m *Manager) runFFmpeg(ctx context.Context, binary, in, out string) error {
	if m.ffmpegFn != nil {
		_, _, err := m.ffmpegFn(ctx, []string{"-y", "-i", in, "-c", "copy", out})
		return err
	}
	// "-c copy" remuxes without re-encoding: cheap, lossless, and usually
	// all that is needed to make a container Chromium understands.
	_, _, err := runExec(ctx, binary, []string{"-y", "-i", in, "-c", "copy", out}, m.opts.Timeout, nil)
	return err
}

// runExec runs a command with a bounded timeout and captures both streams.
// No shell is involved: argv comes from vetted Go strings only.
func runExec(ctx context.Context, binary string, args []string, timeout time.Duration, onLine func(string)) ([]byte, []byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	lw := &lineWriter{onLine: onLine}
	var stderr strings.Builder
	cmd.Stdout = lw
	cmd.Stderr = &stderr
	err := cmd.Run()
	lw.flush()
	return []byte(lw.buf.String()), []byte(stderr.String()), normalizeExecError(err, ctx)
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
