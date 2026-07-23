package hap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ValidationError carries HAProxy's own diagnostics from `haproxy -c`.
type ValidationError struct {
	Output string
	Err    error
}

func (e *ValidationError) Error() string {
	if e.Output != "" {
		return e.Output
	}
	return e.Err.Error()
}

func (e *ValidationError) Unwrap() error { return e.Err }

// Validator runs configuration checks with the managed HAProxy binary.
type Validator struct {
	Binary string
}

// Check writes content to a temporary file and runs `haproxy -c` against it.
// The returned string is HAProxy's combined output, shown verbatim in the UI
// because its line numbers refer to the generated file.
func (v *Validator) Check(ctx context.Context, content string, extraFiles map[string]string) (string, error) {
	if _, err := os.Stat(v.Binary); err != nil {
		return "", fmt.Errorf("haproxy binary not found at %s: run scripts/build-haproxy.sh or set haproxy_bin", v.Binary)
	}

	dir, err := os.MkdirTemp("", "haproxy-check-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	// Extra files (a candidate error page, for example) let the caller check a
	// change before it touches the live tree.
	for name, body := range extraFiles {
		path := filepath.Join(dir, filepath.Base(name))
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return "", err
		}
	}

	cfgPath := filepath.Join(dir, "haproxy.cfg")
	if err := os.WriteFile(cfgPath, []byte(content), 0o640); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, v.Binary, "-c", "-f", cfgPath)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	// HAProxy prints the temp path in diagnostics; rewrite it so the message
	// reads as though it came from the deployed file.
	text = strings.ReplaceAll(text, cfgPath, "haproxy.cfg")

	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return text, &ValidationError{Output: "configuration check timed out after 30s", Err: err}
		}
		return text, &ValidationError{Output: text, Err: err}
	}
	return text, nil
}

// Version returns the `haproxy -v` banner, or an error when the binary is
// missing or unusable.
func (v *Validator) Version(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, v.Binary, "-v").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("run %s -v: %w", v.Binary, err)
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	return strings.TrimSpace(line), nil
}
