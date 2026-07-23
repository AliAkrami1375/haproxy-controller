package hap

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Process manager kinds, as reported by ProcessManager.Kind.
const (
	KindCommand    = "command"
	KindSupervised = "supervised"
)

// ProcessManager starts, reloads and inspects the HAProxy process.
//
// Two implementations exist because the controller runs in two very different
// environments. On a systemd host it shells out to systemctl; inside a
// container it supervises HAProxy itself as a child process, which removes any
// need for an init system or a second image.
type ProcessManager interface {
	// Reload applies the configuration on disk without dropping connections.
	// It starts HAProxy if it is not running yet.
	Reload(ctx context.Context) (string, error)

	// Restart stops and starts HAProxy, dropping existing connections.
	Restart(ctx context.Context) (string, error)

	// Status reports a human-readable state and whether HAProxy is running.
	Status(ctx context.Context) (string, bool)

	// Kind names the strategy, for display in the UI.
	Kind() string
}

// CommandManager drives HAProxy through configured shell commands, which is
// how the controller integrates with systemd or any other init system.
type CommandManager struct {
	ReloadCommand  string
	RestartCommand string
	StatusCommand  string
}

// Kind implements ProcessManager.
func (c *CommandManager) Kind() string { return KindCommand }

// Reload implements ProcessManager.
func (c *CommandManager) Reload(ctx context.Context) (string, error) {
	return runShell(ctx, c.ReloadCommand, 60*time.Second)
}

// Restart implements ProcessManager.
func (c *CommandManager) Restart(ctx context.Context) (string, error) {
	return runShell(ctx, c.RestartCommand, 90*time.Second)
}

// Status implements ProcessManager.
func (c *CommandManager) Status(ctx context.Context) (string, bool) {
	out, err := runShell(ctx, c.StatusCommand, 10*time.Second)
	if out == "" && err != nil {
		out = err.Error()
	}
	return out, err == nil
}

// runShell executes a configured command and returns its combined output.
func runShell(ctx context.Context, command string, timeout time.Duration) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "", fmt.Errorf("no command is configured")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
