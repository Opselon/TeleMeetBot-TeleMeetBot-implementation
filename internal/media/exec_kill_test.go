package media

import (
	bufio "bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestRunExecKillsProcessGroup is the regression test for the subprocess
// cleanup of the yt-dlp/ffmpeg invocations. runExec used exec.CommandContext
// without a process group, so on timeout or cancellation Go SIGKILLed only
// yt-dlp's own PID: an ffmpeg child kept transcoding, holding the output
// file open (so the cleanup could not remove it) and burning CPU after the
// fetch had already been reported as failed.
//
// A shell script stands in for yt-dlp: it spawns a child, records the
// child's PID, and the child parks. The command is cancelled; the child
// must be gone too.
func TestRunExecKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are not the Windows mechanism")
	}

	dir := t.TempDir()
	childPIDFile := filepath.Join(dir, "child.pid")
	aliveFile := filepath.Join(dir, "alive")

	// "yt-dlp": spawns a child that outlives it, reports the child's pid,
	// then exits. The child parks until it is killed.
	script := filepath.Join(dir, "fake-ytdlp.sh")
	scriptSrc := fmt.Sprintf(`#!/bin/sh
echo alive > %s
sleep 300 &
echo $! > %s
wait
`, aliveFile, childPIDFile)
	if err := os.WriteFile(script, []byte(scriptSrc), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(700 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, _, err := runExec(ctx, script, nil, 0, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("runExec returned nil error despite cancellation")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("runExec took %s, expected a fast cancellation", elapsed.Round(time.Millisecond))
	}

	// The child's PID must be recorded and the child must be gone: kill 0
	// probes existence without delivering a signal.
	pidBytes, err := os.ReadFile(childPIDFile)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	pid := strings.TrimSpace(string(pidBytes))
	if pid == "" {
		t.Fatalf("no child pid recorded")
	}
	var n int
	if _, err := fmt.Sscanf(pid, "%d", &n); err != nil || n <= 0 {
		t.Fatalf("recorded pid %q is not a pid", pid)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		// Signal(0) reports EPROCESS-terminated for a dead process; a live
		// one accepts the probe.
		if err := syscall.Kill(n, 0); err != nil {
			return // the child is gone: the group kill did its job
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d is still alive after the parent was cancelled — the process group was not killed", n)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestBoundedWriterBoundsMemory pins the output cap: a hostile source
// streaming an enormous progress log must not grow the captured buffers
// without limit.
func TestBoundedWriterBoundsMemory(t *testing.T) {
	var out strings.Builder
	bw := &boundedWriter{w: &out, max: maxExecOutputBytes}

	// Write twice the cap in chunks: the stored content must stay at the cap
	// and the writer must keep accepting writes (never stall the pipe).
	chunk := bufio.Repeat([]byte("x"), 64*1024)
	for i := 0; i < (maxExecOutputBytes/len(chunk))*2; i++ {
		n, err := bw.Write(chunk)
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if n <= 0 {
			t.Fatalf("write %d returned %d bytes", i, n)
		}
	}
	if out.Len() != maxExecOutputBytes {
		t.Fatalf("bounded writer stored %d bytes, expected exactly the %d cap", out.Len(), maxExecOutputBytes)
	}
}

// TestRunExecStillCapturesOutput keeps the happy path honest: bounded output
// must not break capturing a normal command's stdout and stderr.
func TestRunExecStillCapturesOutput(t *testing.T) {
	bin := "/bin/sh"
	if _, err := os.Stat(bin); err != nil {
		t.Skip("no POSIX shell available")
	}
	stdout, stderr, err := runExec(context.Background(), bin, []string{"-c", "echo out; echo err 1>&2"}, time.Second, nil)
	if err != nil {
		t.Fatalf("runExec: %v", err)
	}
	if !strings.Contains(string(stdout), "out") {
		t.Fatalf("stdout = %q", string(stdout))
	}
	if !strings.Contains(string(stderr), "err") {
		t.Fatalf("stderr = %q", string(stderr))
	}
}
