package svcinst

import (
	"os/exec"
	"strings"
)

// execOutput runs a command and returns its trimmed combined output.
func execOutput(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// trim removes surrounding whitespace.
func trim(s string) string { return strings.TrimSpace(s) }
