package hap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Supervisor runs HAProxy as a direct child of the controller in
// master-worker mode. It is the process manager used inside a container,
// where there is no init system and everything must live in one image.
//
// HAProxy is started with -W (master-worker) and -db (stay in the
// foreground). Reloading is a SIGUSR2 to the master, which spawns a new
// worker on the new configuration and lets the old one drain, so a reload
// never drops a connection.
type Supervisor struct {
	Binary     string
	ConfigPath string
	PIDFile    string
	MasterSock string
	ExtraArgs  []string
	Logger     *slog.Logger

	mu       sync.Mutex
	reloadMu sync.Mutex
	cmd      *exec.Cmd
	started  time.Time
	exited   chan struct{}
	lastErr  string
	output   *ringBuffer
	stopped  bool // set by Shutdown so the watcher does not report a crash
}

// NewSupervisor builds a supervisor for the given binary and config path.
func NewSupervisor(binary, configPath, pidFile, masterSock string, logger *slog.Logger) *Supervisor {
	s := &Supervisor{
		Binary:     binary,
		ConfigPath: configPath,
		PIDFile:    pidFile,
		MasterSock: masterSock,
		Logger:     logger,
		output:     newRingBuffer(200),
	}
	s.output.mirror = func(line string) {
		s.log().Info("haproxy", "line", line)
	}
	return s
}

// Kind implements ProcessManager.
func (s *Supervisor) Kind() string { return KindSupervised }

// Running reports whether the HAProxy master is currently alive.
func (s *Supervisor) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runningLocked()
}

func (s *Supervisor) runningLocked() bool {
	if s.cmd == nil || s.cmd.Process == nil {
		return false
	}
	select {
	case <-s.exited:
		return false
	default:
		return true
	}
}

// Start launches HAProxy if it is not already running.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startLocked(ctx)
}

func (s *Supervisor) startLocked(ctx context.Context) error {
	if s.runningLocked() {
		return nil
	}
	if _, err := os.Stat(s.Binary); err != nil {
		return fmt.Errorf("haproxy binary not found at %s", s.Binary)
	}
	if _, err := os.Stat(s.ConfigPath); err != nil {
		return fmt.Errorf("no configuration at %s yet: apply one from the panel first", s.ConfigPath)
	}

	args := []string{
		"-W",  // master-worker: one master supervising the workers
		"-db", // stay in the foreground so we remain the parent
		"-f", s.ConfigPath,
	}
	if s.PIDFile != "" {
		args = append(args, "-p", s.PIDFile)
	}
	if s.MasterSock != "" {
		args = append(args, "-S", s.MasterSock)
	}
	args = append(args, s.ExtraArgs...)

	cmd := exec.Command(s.Binary, args...)
	cmd.Stdout = s.output
	cmd.Stderr = s.output
	// Give HAProxy its own process group so a signal aimed at the controller
	// does not race us to the workers.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start haproxy: %w", err)
	}

	s.cmd = cmd
	s.started = time.Now()
	s.stopped = false
	s.lastErr = ""
	exited := make(chan struct{})
	s.exited = exited

	go s.watch(cmd, exited)

	s.log().Info("haproxy started", "pid", cmd.Process.Pid, "config", s.ConfigPath)
	return nil
}

// watch reaps the child and records why it went away.
func (s *Supervisor) watch(cmd *exec.Cmd, exited chan struct{}) {
	err := cmd.Wait()
	close(exited)

	s.mu.Lock()
	stopped := s.stopped
	if err != nil {
		s.lastErr = err.Error()
	}
	s.mu.Unlock()

	if stopped {
		s.log().Info("haproxy stopped")
		return
	}
	s.log().Error("haproxy exited unexpectedly",
		"error", err, "output", s.output.Tail(12))
}

// Reload sends SIGUSR2 to the master, which re-reads the configuration and
// hands traffic to a fresh worker. HAProxy is started if it is not running,
// which is what happens on the very first apply.
func (s *Supervisor) Reload(ctx context.Context) (string, error) {
	// One reload at a time: overlapping SIGUSR2 signals make the master's
	// state, and any judgement about it, unreliable.
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	s.mu.Lock()

	if !s.runningLocked() {
		// Nothing to signal yet: this is the first apply, or HAProxy died.
		err := s.startLocked(ctx)
		s.mu.Unlock()
		if err != nil {
			return s.output.Tail(20), err
		}
		return s.waitHealthy(ctx, "started", procState{}, false)
	}

	pid := s.cmd.Process.Pid
	mark := s.output.Len()
	s.mu.Unlock()

	// Record the master's failed-reload count so a new failure is detectable.
	baseline, _ := s.showProc(ctx)

	if err := syscall.Kill(pid, syscall.SIGUSR2); err != nil {
		return s.output.Since(mark), fmt.Errorf("signal haproxy master (pid %d): %w", pid, err)
	}
	s.log().Info("haproxy reload signalled", "pid", pid)
	return s.waitHealthy(ctx, "reloaded", baseline, true)
}

// procState is what the master CLI reports about the running processes.
type procState struct {
	Reloads        int
	FailedReloads  int
	CurrentWorkers int
	// WorkerPIDs identifies the workers currently serving traffic. Comparing
	// this set across a reload is the only reliable way to tell that a new
	// worker actually took over.
	WorkerPIDs []int
}

// newWorkersSince returns the workers present in st but not in baseline.
func (st procState) newWorkersSince(baseline procState) []int {
	old := make(map[int]bool, len(baseline.WorkerPIDs))
	for _, pid := range baseline.WorkerPIDs {
		old[pid] = true
	}
	var fresh []int
	for _, pid := range st.WorkerPIDs {
		if !old[pid] {
			fresh = append(fresh, pid)
		}
	}
	return fresh
}

// hasNewWorker reports whether st contains a worker that was not present in
// the given baseline.
func (st procState) hasNewWorker(baseline procState) bool {
	return len(st.newWorkersSince(baseline)) > 0
}

// containsAny reports whether any of the given workers is still current.
func (st procState) containsAny(pids []int) bool {
	live := make(map[int]bool, len(st.WorkerPIDs))
	for _, pid := range st.WorkerPIDs {
		live[pid] = true
	}
	for _, pid := range pids {
		if live[pid] {
			return true
		}
	}
	return false
}

// masterCommand sends one command to HAProxy's master CLI socket.
func (s *Supervisor) masterCommand(ctx context.Context, cmd string) (string, error) {
	if strings.TrimSpace(s.MasterSock) == "" {
		return "", fmt.Errorf("no master socket configured")
	}
	deadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", s.MasterSock)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	if _, err := io.WriteString(conn, cmd+"\n"); err != nil {
		return "", err
	}
	return readCLIResponse(conn, deadline)
}

// readCLIResponse reads one reply from a HAProxy CLI socket.
//
// The master CLI keeps the connection open after answering instead of sending
// EOF, so io.ReadAll would block until the deadline and then discard a
// perfectly good reply as a timeout. Reading until the output goes idle works
// whether or not the far end closes.
func readCLIResponse(conn net.Conn, deadline time.Time) (string, error) {
	const idle = 400 * time.Millisecond

	var buf strings.Builder
	chunk := make([]byte, 4096)

	for {
		readBy := time.Now().Add(idle)
		if readBy.After(deadline) {
			readBy = deadline
		}
		_ = conn.SetReadDeadline(readBy)

		n, err := conn.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
		}
		if err == nil {
			if time.Now().After(deadline) {
				return buf.String(), nil
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			return buf.String(), nil
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			// Output has gone quiet. Anything received is the full reply.
			if buf.Len() > 0 {
				return buf.String(), nil
			}
			if time.Now().After(deadline) {
				return "", fmt.Errorf("no response from the haproxy master CLI")
			}
			continue
		}
		return buf.String(), err
	}
}

// showProc parses `show proc` from the master CLI.
//
// The output looks like:
//
//	#<PID>   <type>    <reloads>       <uptime>      <version>
//	35       master    5 [failed: 1]   0d00h02m31s   3.2.21
//	# workers
//	119      worker    0               0d00h01m12s   3.2.21
//	# old workers
//	# programs
//
// An empty "# workers" section means the most recent reload produced no
// serving worker, which is the case this whole check exists to catch.
func (s *Supervisor) showProc(ctx context.Context) (procState, error) {
	out, err := s.masterCommand(ctx, "show proc")
	if err != nil {
		return procState{}, err
	}
	return s.parseShowProc(out), nil
}

// parseShowProc extracts the process state from master CLI output.
func (s *Supervisor) parseShowProc(out string) procState {
	var st procState
	section := "master"
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			switch {
			case strings.HasPrefix(trimmed, "# workers"):
				section = "workers"
			case strings.HasPrefix(trimmed, "# old workers"):
				section = "old"
			case strings.HasPrefix(trimmed, "# programs"):
				section = "programs"
			}
			continue // also skips the column header, which starts with "#<PID>"
		}

		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		switch {
		case fields[1] == "master":
			// Column 3 is the reload count, optionally followed by a
			// "[failed: N]" suffix that is split across two fields.
			if len(fields) > 2 {
				st.Reloads = atoi(fields[2])
			}
			if i := strings.Index(trimmed, "[failed:"); i >= 0 {
				rest := trimmed[i+len("[failed:"):]
				if j := strings.Index(rest, "]"); j >= 0 {
					st.FailedReloads = atoi(strings.TrimSpace(rest[:j]))
				}
			}
		case section == "workers" && fields[1] == "worker":
			st.CurrentWorkers++
			if pid := atoi(fields[0]); pid > 0 {
				st.WorkerPIDs = append(st.WorkerPIDs, pid)
			}
		}
	}
	return st
}

// waitHealthy confirms the reload actually produced a serving worker.
//
// Checking only that the master is alive is not enough: when a new worker
// cannot start -- a bind address that does not exist on this host, a port
// already taken, a missing certificate file -- the master survives and the
// previous worker keeps serving the OLD configuration. Without this check the
// controller would report success while the change silently never took effect.
func (s *Supervisor) waitHealthy(ctx context.Context, action string, baseline procState, expectReload bool) (string, error) {
	// The master forks the new worker and lists it as current straight away,
	// then only reports the failure about a second later when that worker
	// exits. So a new worker appearing proves nothing on its own: it has to
	// still be there after a settle window.
	const settle = 2500 * time.Millisecond

	deadline := time.Now().Add(15 * time.Second)
	var (
		fresh   []int
		settled time.Time
	)

	for {
		if !s.Running() {
			return s.output.Tail(20), fmt.Errorf("haproxy is not running after being %s", action)
		}

		st, err := s.showProc(ctx)
		switch {
		case err == nil:
			if st.FailedReloads > baseline.FailedReloads {
				return s.output.Tail(20), fmt.Errorf(
					"haproxy could not start a worker on the new configuration and kept the previous one " +
						"running; the change did not take effect")
			}

			if fresh == nil {
				if !expectReload && st.CurrentWorkers > 0 {
					fresh = st.WorkerPIDs
					settled = time.Now().Add(settle)
				} else if expectReload && st.hasNewWorker(baseline) {
					fresh = st.newWorkersSince(baseline)
					settled = time.Now().Add(settle)
				}
			} else if !st.containsAny(fresh) {
				return s.output.Tail(20), fmt.Errorf(
					"the worker haproxy started on the new configuration exited immediately; " +
						"the change did not take effect")
			}

			if fresh != nil && time.Now().After(settled) {
				return s.output.Tail(10), nil
			}

		case strings.Contains(err.Error(), "no master socket configured"):
			// Without a master CLI there is nothing better than a liveness
			// check, so fall back to it rather than failing every reload.
			time.Sleep(settle)
			if !s.Running() {
				return s.output.Tail(20), fmt.Errorf("haproxy is not running after being %s", action)
			}
			return s.output.Tail(10), nil
		}

		if time.Now().After(deadline) {
			if err != nil {
				return s.output.Tail(20), fmt.Errorf("could not confirm haproxy is serving after being %s: %w", action, err)
			}
			return s.output.Tail(20), fmt.Errorf(
				"haproxy did not start a worker on the new configuration within the timeout; "+
					"the change did not take effect (action: %s)", action)
		}
		select {
		case <-ctx.Done():
			return s.output.Tail(20), ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Restart stops HAProxy and starts it again, dropping existing connections.
func (s *Supervisor) Restart(ctx context.Context) (string, error) {
	if err := s.stop(ctx, 20*time.Second); err != nil {
		return s.output.Tail(20), err
	}
	s.mu.Lock()
	err := s.startLocked(ctx)
	s.mu.Unlock()
	if err != nil {
		return s.output.Tail(20), err
	}
	return s.waitHealthy(ctx, "restarted", procState{}, false)
}

// Status implements ProcessManager.
func (s *Supervisor) Status(ctx context.Context) (string, bool) {
	s.mu.Lock()
	running := s.runningLocked()
	var pid int
	if running && s.cmd != nil && s.cmd.Process != nil {
		pid = s.cmd.Process.Pid
	}
	uptime := time.Since(s.started).Truncate(time.Second)
	lastErr := s.lastErr
	s.mu.Unlock()

	if running {
		return fmt.Sprintf("active (pid %d, up %s)", pid, uptime), true
	}
	if lastErr != "" {
		return "inactive (" + lastErr + ")", false
	}
	return "inactive", false
}

// stop asks HAProxy to finish gracefully, escalating to SIGKILL if it will
// not leave within the grace period.
func (s *Supervisor) stop(ctx context.Context, grace time.Duration) error {
	s.mu.Lock()
	if !s.runningLocked() {
		s.mu.Unlock()
		return nil
	}
	pid := s.cmd.Process.Pid
	exited := s.exited
	s.stopped = true
	s.mu.Unlock()

	// SIGUSR1 is HAProxy's soft stop: listeners close, in-flight requests
	// are allowed to finish.
	if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}

	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-exited:
		return nil
	case <-timer.C:
	}

	s.log().Warn("haproxy did not stop within the grace period; killing", "pid", pid)
	_ = syscall.Kill(-pid, syscall.SIGKILL)

	kill := time.NewTimer(5 * time.Second)
	defer kill.Stop()
	select {
	case <-exited:
		return nil
	case <-kill.C:
		return fmt.Errorf("haproxy (pid %d) could not be stopped", pid)
	}
}

// Shutdown stops HAProxy as part of the controller shutting down.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	return s.stop(ctx, 25*time.Second)
}

// Output returns the most recent lines HAProxy has written, for the UI.
func (s *Supervisor) Output(n int) string { return s.output.Tail(n) }

func (s *Supervisor) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// ringBuffer keeps the last N lines written by the child process. HAProxy can
// be chatty, and an unbounded buffer in a long-running container is a leak.
type ringBuffer struct {
	mu      sync.Mutex
	lines   []string
	max     int
	partial string
	total   int

	// mirror receives every complete line as well, so HAProxy's own
	// diagnostics show up in the controller log and in `docker logs`.
	mirror func(line string)
}

func newRingBuffer(max int) *ringBuffer {
	return &ringBuffer{max: max, lines: make([]string, 0, max)}
}

// Write implements io.Writer, splitting the stream into whole lines.
func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.partial += string(p)
	for {
		idx := strings.IndexByte(r.partial, '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimRight(r.partial[:idx], "\r")
		r.partial = r.partial[idx+1:]
		r.appendLocked(line)
	}
	// Guard against a pathological line with no newline at all.
	if len(r.partial) > 8192 {
		r.appendLocked(r.partial)
		r.partial = ""
	}
	return len(p), nil
}

func (r *ringBuffer) appendLocked(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	if r.mirror != nil {
		r.mirror(line)
	}
	r.total++
	if len(r.lines) == r.max {
		copy(r.lines, r.lines[1:])
		r.lines[r.max-1] = line
		return
	}
	r.lines = append(r.lines, line)
}

// Tail returns the last n lines.
func (r *ringBuffer) Tail(n int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || len(r.lines) == 0 {
		return ""
	}
	if n > len(r.lines) {
		n = len(r.lines)
	}
	return strings.Join(r.lines[len(r.lines)-n:], "\n")
}

// Len returns how many lines have been written in total, used as a marker so
// a caller can read just the output produced by its own action.
func (r *ringBuffer) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

// Since returns the lines written after the given marker.
func (r *ringBuffer) Since(mark int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	produced := r.total - mark
	if produced <= 0 || len(r.lines) == 0 {
		return ""
	}
	if produced > len(r.lines) {
		produced = len(r.lines)
	}
	return strings.Join(r.lines[len(r.lines)-produced:], "\n")
}
