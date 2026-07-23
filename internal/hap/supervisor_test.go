package hap

import "testing"

// parseProc runs the same parsing showProc does, on captured master CLI output.
func parseProc(t *testing.T, out string) procState {
	t.Helper()
	s := &Supervisor{}
	return s.parseShowProc(out)
}

func TestParseShowProc(t *testing.T) {
	// A healthy master with one serving worker.
	healthy := `#<PID>          <type>          <reloads>       <uptime>        <version>
35              master          2               0d00h05m10s     3.2.21
# workers
119             worker          0               0d00h01m12s     3.2.21
# old workers
# programs
`
	st := parseProc(t, healthy)
	if st.Reloads != 2 {
		t.Errorf("Reloads = %d, want 2", st.Reloads)
	}
	if st.CurrentWorkers != 1 {
		t.Errorf("CurrentWorkers = %d, want 1", st.CurrentWorkers)
	}
	if st.FailedReloads != 0 {
		t.Errorf("FailedReloads = %d, want 0", st.FailedReloads)
	}

	// The failure this check exists for: the new worker never started, so the
	// workers section is empty while the previous worker still serves.
	failed := `#<PID>          <type>          <reloads>       <uptime>        <version>
35              master          5 [failed: 1]   0d00h02m31s     3.2.21
# workers
# old workers
119             worker          1               0d00h01m12s     3.2.21
# programs
`
	st = parseProc(t, failed)
	if st.Reloads != 5 {
		t.Errorf("Reloads = %d, want 5 (parsed alongside the [failed: N] suffix)", st.Reloads)
	}
	if st.CurrentWorkers != 0 {
		t.Errorf("CurrentWorkers = %d, want 0 (the old worker must not count)", st.CurrentWorkers)
	}
	if st.FailedReloads != 1 {
		t.Errorf("FailedReloads = %d, want 1", st.FailedReloads)
	}

	// Several workers, as seen with nbproc-style setups.
	multi := `#<PID>          <type>          <reloads>       <uptime>        <version>
7               master          0               0d00h00m05s     3.2.21
# workers
9               worker          0               0d00h00m05s     3.2.21
10              worker          0               0d00h00m05s     3.2.21
# old workers
# programs
`
	if st := parseProc(t, multi); st.CurrentWorkers != 2 {
		t.Errorf("CurrentWorkers = %d, want 2", st.CurrentWorkers)
	}

	// Garbage must not panic or invent workers.
	if st := parseProc(t, "not proc output at all\n"); st.CurrentWorkers != 0 || st.FailedReloads != 0 {
		t.Errorf("unexpected state from junk input: %+v", st)
	}
}

// A reload is only successful once a worker that did not exist before is
// serving; the previous worker stays listed as current until then.
func TestHasNewWorker(t *testing.T) {
	before := procState{CurrentWorkers: 1, WorkerPIDs: []int{119}}

	if before.hasNewWorker(before) {
		t.Error("the same worker set must not count as a new worker")
	}
	after := procState{CurrentWorkers: 1, WorkerPIDs: []int{140}}
	if !after.hasNewWorker(before) {
		t.Error("a different worker pid must count as a new worker")
	}
	// During a handover both may be listed.
	both := procState{CurrentWorkers: 2, WorkerPIDs: []int{119, 140}}
	if !both.hasNewWorker(before) {
		t.Error("a set containing a new pid must count as a new worker")
	}
	// First start: no baseline at all.
	if !after.hasNewWorker(procState{}) {
		t.Error("any worker is new when there is no baseline")
	}
}

// The failure mode this guards against: the master lists the freshly forked
// worker as current, then that worker exits a moment later.
func TestWorkerDisappearsAfterAppearing(t *testing.T) {
	baseline := procState{CurrentWorkers: 1, WorkerPIDs: []int{91}}
	appeared := procState{CurrentWorkers: 2, WorkerPIDs: []int{91, 216}}

	fresh := appeared.newWorkersSince(baseline)
	if len(fresh) != 1 || fresh[0] != 216 {
		t.Fatalf("newWorkersSince = %v, want [216]", fresh)
	}
	// A moment later the new worker is gone and only the old one remains.
	gone := procState{CurrentWorkers: 1, WorkerPIDs: []int{91}}
	if gone.containsAny(fresh) {
		t.Error("containsAny must report the new worker as gone")
	}
	// If it had survived, it would still be listed.
	survived := procState{CurrentWorkers: 1, WorkerPIDs: []int{216}}
	if !survived.containsAny(fresh) {
		t.Error("containsAny must report a surviving worker as present")
	}
}

func TestParseShowProcCapturesWorkerPIDs(t *testing.T) {
	out := `#<PID>          <type>          <reloads>       <uptime>        <version>
7               master          1               0d00h00m05s     3.2.21
# workers
9               worker          0               0d00h00m05s     3.2.21
10              worker          0               0d00h00m05s     3.2.21
# old workers
11              worker          1               0d00h09m05s     3.2.21
# programs
`
	st := parseProc(t, out)
	if len(st.WorkerPIDs) != 2 || st.WorkerPIDs[0] != 9 || st.WorkerPIDs[1] != 10 {
		t.Errorf("WorkerPIDs = %v, want [9 10] (old workers excluded)", st.WorkerPIDs)
	}
}

func TestRingBufferKeepsLastLines(t *testing.T) {
	r := newRingBuffer(3)
	r.Write([]byte("one\ntwo\nthree\nfour\n"))
	if got := r.Tail(3); got != "two\nthree\nfour" {
		t.Errorf("Tail(3) = %q", got)
	}
	if got := r.Tail(10); got != "two\nthree\nfour" {
		t.Errorf("Tail(10) = %q, want it clamped to what is retained", got)
	}
}

func TestRingBufferSinceMarker(t *testing.T) {
	r := newRingBuffer(10)
	r.Write([]byte("a\nb\n"))
	mark := r.Len()
	r.Write([]byte("c\nd\n"))
	if got := r.Since(mark); got != "c\nd" {
		t.Errorf("Since(mark) = %q, want only the lines written after it", got)
	}
}

// A line arriving in fragments must still be recorded once, whole.
func TestRingBufferHandlesSplitWrites(t *testing.T) {
	r := newRingBuffer(10)
	r.Write([]byte("hel"))
	r.Write([]byte("lo wor"))
	r.Write([]byte("ld\n"))
	if got := r.Tail(1); got != "hello world" {
		t.Errorf("Tail(1) = %q, want %q", got, "hello world")
	}
}
