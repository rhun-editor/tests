package main

// Debug runs: rhun asks for a run with the debug profile, and the extension starts one session of
// its own debuggers (extension.ini: [debugger.go-test], Delve; [debugger.rust-test], lldb-dap) for
// the selected tests with debug-start, within the run (rhun lets a debug-profile run start one).
// The program's output goes to the Debug Console; when the session ends its exit status becomes
// the tests' result.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

// errNothing: the adapter has nothing of the run's to debug (the next one may)
var errNothing = errors.New("nothing to debug")

// sessionEnded delivers a debug-ended event to the run waiting for it, or keeps it for the run
// that has not heard its session's id yet (on the handler's goroutine)
func (x *ext) sessionEnded(d rhunext.DebugEnded) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if ch := x.waiting[d.Session]; ch != nil {
		delete(x.waiting, d.Session)
		ch <- d
		return
	}
	if x.ended == nil {
		x.ended = map[int64]rhunext.DebugEnded{}
	}
	x.ended[d.Session] = d
}

// debugSession starts a session for the run and waits for its end, or for the run's Cancel
// (then ok is false and the session is stopped; its debug-ended lands in the channel nobody reads)
func (x *ext) debugSession(r rhunext.TestRun, name string, cfg rhunext.DebugConfig) (rhunext.DebugEnded, bool, error) {
	st := x.runState(r.Run)
	session, err := x.e.DebugStart(context.Background(), name, cfg)
	if err != nil {
		return rhunext.DebugEnded{}, false, err
	}
	ch := make(chan rhunext.DebugEnded, 1)
	x.mu.Lock()
	if d, done := x.ended[session]; done {
		delete(x.ended, session)
		x.mu.Unlock()
		return d, true, nil
	}
	if x.waiting == nil {
		x.waiting = map[int64]chan rhunext.DebugEnded{}
	}
	x.waiting[session] = ch
	x.mu.Unlock()
	select {
	case d := <-ch:
		return d, true, nil
	case <-st.cancel:
		_ = x.e.DebugStop(context.Background(), session) // a session already over answers ok too
		return rhunext.DebugEnded{}, false, nil
	}
}

// debugResults: the tests' result from the session's end
func debugResults(ids []string, d rhunext.DebugEnded, ok bool) []rhunext.TestResult {
	var rs []rhunext.TestResult
	for _, id := range ids {
		r := rhunext.TestResult{Item: id, Duration: rhunext.NoDuration}
		switch {
		case !ok:
			continue // cancelled: the items go back to what they were
		case d.HasCode && d.Code == 0:
			r.State = rhunext.TestPassed
		case d.HasCode:
			r.State = rhunext.TestFailed
			r.Messages = []rhunext.TestMessage{{Text: fmt.Sprintf("The tests exited with code %d (their output is in the Debug Console)", d.Code)}}
		default:
			r.State = rhunext.TestErrored
			r.Messages = []rhunext.TestMessage{{Text: "The debug session ended before the tests did"}}
		}
		rs = append(rs, r)
	}
	return rs
}

func started(rep *rhunext.TestReporter, ids []string) {
	for _, id := range ids {
		rep.Add(rhunext.TestResult{Item: id, State: rhunext.TestStarted, Duration: rhunext.NoDuration})
	}
}

// ---- Go: Delve in test mode on one package

func (g *goAdapter) debug(x *ext, r rhunext.TestRun, ids []string, rep *rhunext.TestReporter) error {
	jobs := g.jobs(ids)
	var job *goJob
	for i := range jobs {
		if jobs[i].mod != nil {
			job = &jobs[i]
			break
		}
	}
	if job == nil {
		return errNothing
	}
	if len(job.pkgs) != 1 || strings.HasSuffix(job.pkgs[0], "...") {
		return fmt.Errorf("debugging takes one package: choose a package or its tests")
	}
	g.publish(*job, rep)
	dir := filepath.Join(job.mod.dir, filepath.FromSlash(strings.TrimPrefix(job.pkgs[0], "./")))
	var cfg rhunext.DebugConfig
	cfg.Set("type", rhunext.DebugStr("go-test"))
	cfg.Set("request", rhunext.DebugStr("launch"))
	cfg.Set("mode", rhunext.DebugStr("test"))
	cfg.Set("program", rhunext.DebugStr(dir))
	cfg.Set("cwd", rhunext.DebugStr(dir))
	args := []string{"-test.v"}
	switch {
	case job.bench != "":
		args = append(args, "-test.run", "^$", "-test.bench", job.bench)
	case job.pattern != "":
		args = append(args, "-test.run", job.pattern)
	}
	cfg.SetList("args", args)
	if len(jobs) > 1 {
		rep.Add(rhunext.TestResult{Duration: rhunext.NoDuration,
			Output: []byte("Debugging takes one package at a time: the first selected is debugged\n")})
	}
	if len(ids) == 0 {
		ids = []string{"go:" + filepath.ToSlash(strings.TrimPrefix(job.pkgs[0], "./"))}
	}
	started(rep, ids)
	rep.Flush()
	d, ok, err := x.debugSession(r, debugName(ids), cfg)
	if err != nil {
		return fmt.Errorf("debug: %v", err)
	}
	for _, res := range debugResults(ids, d, ok) {
		rep.Add(res)
	}
	return nil
}

// ---- Rust: the test binary cargo builds, under lldb-dap

func (c *cargoAdapter) debug(x *ext, r rhunext.TestRun, ids []string, rep *rhunext.TestReporter) error {
	jobs := c.jobs(ids)
	if len(jobs) == 0 {
		return errNothing
	}
	j := jobs[0]
	c.publish(j, rep)
	// the test binary: cargo builds it and says where (its JSON messages)
	args := []string{"test", "--no-run", "--message-format=json", "-p", j.pkg.name}
	args = append(args, cargoTargetArgs(j.target)...)
	args = append(args, x.flags("cargo_flags")...)
	p, err := x.e.TestExec(r.Run, rhunext.TestCommand{Program: "cargo", Dir: j.pkg.dir, Args: args})
	if err != nil {
		return fmt.Errorf("cargo test --no-run: %v", err)
	}
	errText := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(p.Stderr); errText <- b }()
	out, _ := io.ReadAll(p.Stdout)
	stderr := <-errText
	if code := p.ExitCode(); code != 0 {
		return fmt.Errorf("cargo could not build the tests:\n%s", lastLines(string(stderr), 20))
	}
	exe := testExecutable(out, j.target)
	if exe == "" {
		return fmt.Errorf("cargo built no test binary for %s", j.target)
	}
	var cfg rhunext.DebugConfig
	cfg.Set("type", rhunext.DebugStr("rust-test"))
	cfg.Set("request", rhunext.DebugStr("launch"))
	cfg.Set("program", rhunext.DebugStr(exe))
	cfg.Set("cwd", rhunext.DebugStr(j.pkg.dir))
	targs := []string{"--nocapture", "--test-threads=1"}
	if len(j.tests) > 0 {
		targs = append(targs, "--exact", "--include-ignored")
		targs = append(targs, j.tests...)
	}
	cfg.SetList("args", targs)
	if len(jobs) > 1 {
		rep.Add(rhunext.TestResult{Duration: rhunext.NoDuration,
			Output: []byte("Debugging takes one target at a time: the first selected is debugged\n")})
	}
	if len(ids) == 0 {
		ids = []string{"cargo:" + j.pkg.name + ":" + j.target}
	}
	started(rep, ids)
	rep.Flush()
	d, ok, err := x.debugSession(r, debugName(ids), cfg)
	if err != nil {
		return fmt.Errorf("debug: %v", err)
	}
	for _, res := range debugResults(ids, d, ok) {
		rep.Add(res)
	}
	return nil
}

// cargoTargetArgs selects a target as cargo test does
func cargoTargetArgs(target string) []string {
	switch {
	case target == "lib":
		return []string{"--lib"}
	case target == "bin":
		return []string{"--bins"}
	case strings.HasPrefix(target, "tests/"):
		return []string{"--test", strings.TrimPrefix(target, "tests/")}
	}
	return nil
}

// testExecutable finds the target's test binary in cargo's JSON messages
func testExecutable(out []byte, target string) string {
	for _, line := range strings.Split(string(out), "\n") {
		var m struct {
			Reason     string `json:"reason"`
			Executable string `json:"executable"`
			Profile    struct {
				Test bool `json:"test"`
			} `json:"profile"`
			Target struct {
				Kind []string `json:"kind"`
				Name string   `json:"name"`
			} `json:"target"`
		}
		if json.Unmarshal([]byte(line), &m) != nil || m.Reason != "compiler-artifact" || m.Executable == "" || !m.Profile.Test {
			continue
		}
		kind := strings.Join(m.Target.Kind, " ")
		switch {
		case target == "lib" && (strings.Contains(kind, "lib")):
			return m.Executable
		case target == "bin" && kind == "bin":
			return m.Executable
		case strings.HasPrefix(target, "tests/") && kind == "test" && m.Target.Name == strings.TrimPrefix(target, "tests/"):
			return m.Executable
		}
	}
	return ""
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// debugName: the session's name, after what it debugs
func debugName(ids []string) string {
	id := ids[0]
	for _, sep := range []string{"/", ".", ":"} {
		if i := strings.LastIndex(id, sep); i >= 0 && i < len(id)-1 && sep != "/" {
			id = id[i+1:]
			break
		}
	}
	name := "Debug " + id
	if len(ids) > 1 {
		name += fmt.Sprintf(" and %d more", len(ids)-1)
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}
