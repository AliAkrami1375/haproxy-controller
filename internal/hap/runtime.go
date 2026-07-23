package hap

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Runtime talks to HAProxy's stats socket (the Runtime API).
type Runtime struct {
	Socket string
}

// ErrNoSocket is returned when the stats socket is absent or unreachable,
// which simply means HAProxy is not running yet.
type ErrNoSocket struct{ Err error }

func (e *ErrNoSocket) Error() string { return "stats socket unavailable: " + e.Err.Error() }
func (e *ErrNoSocket) Unwrap() error { return e.Err }

// command sends one Runtime API command and returns the raw reply.
func (r *Runtime) command(ctx context.Context, cmd string) (string, error) {
	if strings.TrimSpace(r.Socket) == "" {
		return "", &ErrNoSocket{Err: fmt.Errorf("no socket configured")}
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", r.Socket)
	if err != nil {
		return "", &ErrNoSocket{Err: err}
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	if _, err := io.WriteString(conn, cmd+"\n"); err != nil {
		return "", err
	}
	data, err := io.ReadAll(conn)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Info holds the fields from `show info` that the dashboard displays.
type Info struct {
	Version        string
	Uptime         string
	Pid            string
	Processes      string
	CurrConns      int
	CumConns       int64
	CumReq         int64
	SessRate       int
	MaxConn        int
	ConnRate       int
	RunQueue       int
	Tasks          int
	MemMaxMB       string
	SslFrontendKey int
}

// ShowInfo returns process-level statistics.
func (r *Runtime) ShowInfo(ctx context.Context) (*Info, error) {
	out, err := r.command(ctx, "show info")
	if err != nil {
		return nil, err
	}

	info := &Info{}
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "Version":
			info.Version = value
		case "Uptime":
			info.Uptime = value
		case "Pid":
			info.Pid = value
		case "Process_num":
			info.Processes = value
		case "CurrConns":
			info.CurrConns = atoi(value)
		case "CumConns":
			info.CumConns = int64(atoi(value))
		case "CumReq":
			info.CumReq = int64(atoi(value))
		case "SessRate":
			info.SessRate = atoi(value)
		case "Maxconn":
			info.MaxConn = atoi(value)
		case "ConnRate":
			info.ConnRate = atoi(value)
		case "Run_queue":
			info.RunQueue = atoi(value)
		case "Tasks":
			info.Tasks = atoi(value)
		case "Memmax_MB":
			info.MemMaxMB = value
		}
	}
	return info, sc.Err()
}

// StatRow is one line of `show stat`, covering a frontend, a backend or a
// single server within a backend.
type StatRow struct {
	ProxyName  string
	SvName     string
	Type       string // frontend | backend | server | socket
	Status     string
	Weight     int
	CurrentSes int
	MaxSes     int
	TotalSes   int64
	BytesIn    int64
	BytesOut   int64
	ErrorReq   int64
	ErrorConn  int64
	ErrorResp  int64
	Retries    int64
	Downtime   int64
	LastChange int64
	CheckStat  string
	Addr       string
	Mode       string
	Rate       int
	HTTP2xx    int64
	HTTP3xx    int64
	HTTP4xx    int64
	HTTP5xx    int64
}

// IsUp reports whether the row is in a healthy state.
func (s StatRow) IsUp() bool {
	switch {
	case strings.HasPrefix(s.Status, "UP"), s.Status == "OPEN", s.Status == "no check":
		return true
	}
	return false
}

// IsDown reports whether the row is in a failed state.
func (s StatRow) IsDown() bool {
	return strings.HasPrefix(s.Status, "DOWN") || s.Status == "MAINT" || s.Status == "NOLB"
}

// StatusClass maps a status to a CSS class used by the dashboard.
func (s StatRow) StatusClass() string {
	switch {
	case s.IsUp():
		return "up"
	case s.Status == "MAINT":
		return "maint"
	case s.IsDown():
		return "down"
	}
	return "unknown"
}

// ShowStat parses `show stat` into typed rows. HAProxy emits CSV with a
// leading "# " on the header line.
func (r *Runtime) ShowStat(ctx context.Context) ([]StatRow, error) {
	out, err := r.command(ctx, "show stat")
	if err != nil {
		return nil, err
	}

	body := strings.TrimPrefix(strings.TrimSpace(out), "# ")
	reader := csv.NewReader(strings.NewReader(body))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse stats: %w", err)
	}
	if len(records) < 2 {
		return nil, nil
	}

	// Address fields by name: HAProxy has added columns over the years and
	// fixed indexes would break across versions.
	idx := map[string]int{}
	for i, name := range records[0] {
		idx[strings.TrimSpace(name)] = i
	}
	get := func(rec []string, name string) string {
		i, ok := idx[name]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	var rows []StatRow
	for _, rec := range records[1:] {
		if len(rec) < 2 || strings.TrimSpace(rec[0]) == "" {
			continue
		}
		row := StatRow{
			ProxyName:  get(rec, "pxname"),
			SvName:     get(rec, "svname"),
			Status:     get(rec, "status"),
			Weight:     atoi(get(rec, "weight")),
			CurrentSes: atoi(get(rec, "scur")),
			MaxSes:     atoi(get(rec, "smax")),
			TotalSes:   atoi64(get(rec, "stot")),
			BytesIn:    atoi64(get(rec, "bin")),
			BytesOut:   atoi64(get(rec, "bout")),
			ErrorReq:   atoi64(get(rec, "ereq")),
			ErrorConn:  atoi64(get(rec, "econ")),
			ErrorResp:  atoi64(get(rec, "eresp")),
			Retries:    atoi64(get(rec, "wretr")),
			Downtime:   atoi64(get(rec, "downtime")),
			LastChange: atoi64(get(rec, "lastchg")),
			CheckStat:  get(rec, "check_status"),
			Addr:       get(rec, "addr"),
			Mode:       get(rec, "mode"),
			Rate:       atoi(get(rec, "rate")),
			HTTP2xx:    atoi64(get(rec, "hrsp_2xx")),
			HTTP3xx:    atoi64(get(rec, "hrsp_3xx")),
			HTTP4xx:    atoi64(get(rec, "hrsp_4xx")),
			HTTP5xx:    atoi64(get(rec, "hrsp_5xx")),
		}

		// The `type` column is numeric: 0 frontend, 1 backend, 2 server, 3 socket.
		switch get(rec, "type") {
		case "0":
			row.Type = "frontend"
		case "1":
			row.Type = "backend"
		case "2":
			row.Type = "server"
		case "3":
			row.Type = "socket"
		default:
			switch row.SvName {
			case "FRONTEND":
				row.Type = "frontend"
			case "BACKEND":
				row.Type = "backend"
			default:
				row.Type = "server"
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// serverToken validates a "backend/server" pair before it is interpolated
// into a Runtime API command.
func serverToken(backend, server string) (string, error) {
	for _, v := range []string{backend, server} {
		if v == "" || strings.ContainsAny(v, " \t\n\r/") {
			return "", fmt.Errorf("invalid backend or server name")
		}
	}
	return backend + "/" + server, nil
}

// SetServerState puts a server into ready, drain or maint via the Runtime API,
// taking effect immediately without a reload.
func (r *Runtime) SetServerState(ctx context.Context, backend, server, state string) error {
	switch state {
	case "ready", "drain", "maint":
	default:
		return fmt.Errorf("unknown server state %q", state)
	}
	token, err := serverToken(backend, server)
	if err != nil {
		return err
	}
	out, err := r.command(ctx, fmt.Sprintf("set server %s state %s", token, state))
	if err != nil {
		return err
	}
	return runtimeError(out)
}

// SetServerWeight changes a server's weight at runtime.
func (r *Runtime) SetServerWeight(ctx context.Context, backend, server string, weight int) error {
	if weight < 0 || weight > 256 {
		return fmt.Errorf("weight %d is out of range (0-256)", weight)
	}
	token, err := serverToken(backend, server)
	if err != nil {
		return err
	}
	out, err := r.command(ctx, fmt.Sprintf("set weight %s %d", token, weight))
	if err != nil {
		return err
	}
	return runtimeError(out)
}

// runtimeError converts a non-empty Runtime API reply into an error. On
// success HAProxy answers with an empty line.
func runtimeError(out string) error {
	msg := strings.TrimSpace(out)
	if msg == "" {
		return nil
	}
	low := strings.ToLower(msg)
	if strings.Contains(low, "unknown") || strings.Contains(low, "no such") ||
		strings.Contains(low, "denied") || strings.Contains(low, "not found") ||
		strings.Contains(low, "invalid") || strings.Contains(low, "requires") {
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// Available reports whether the stats socket can be reached.
func (r *Runtime) Available(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := r.command(ctx, "show info")
	return err == nil
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return n
}
