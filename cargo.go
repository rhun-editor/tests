package main

// The Cargo adapter. Items: a package (cargo:PKG), its test targets (cargo:PKG:lib, cargo:PKG:bin,
// cargo:PKG:tests/NAME), and their tests (cargo:PKG:TARGET:module::path::name) found by scanning the
// sources for #[test] functions. A run is one `cargo test` per target; stable libtest prints
// "test NAME ... ok|FAILED|ignored" and each failure's output under "---- NAME stdout ----", where
// "panicked at FILE:L:C:" gives the place and assert_eq!'s left:/right: the values.

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

type cargoPkg struct {
	name string
	dir  string
}

type cargoAdapter struct {
	mu       sync.Mutex
	pkgs     map[string]*cargoPkg
	resolved map[string]bool // targets whose tests were given
}

func newCargoAdapter() *cargoAdapter {
	return &cargoAdapter{pkgs: map[string]*cargoPkg{}, resolved: map[string]bool{}}
}

func (c *cargoAdapter) owns(id string) bool { return strings.HasPrefix(id, "cargo:") }

var rePkgName = regexp.MustCompile(`(?m)^\s*name\s*=\s*"([^"]+)"`)

func (c *cargoAdapter) roots(project string) []rhunext.TestItem {
	var items []rhunext.TestItem
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, dir := range walk(project, "Cargo.toml", 200) {
		src, err := os.ReadFile(filepath.Join(dir, "Cargo.toml"))
		if err != nil {
			continue
		}
		// the name in [package] (a workspace's own Cargo.toml without one is no package)
		at := strings.Index(string(src), "[package]")
		if at < 0 {
			continue
		}
		body := string(src[at+len("[package]"):])
		if next := strings.Index(body, "\n["); next >= 0 {
			body = body[:next]
		}
		m := rePkgName.FindStringSubmatch(body)
		if m == nil {
			continue
		}
		p := &cargoPkg{name: m[1], dir: dir}
		c.pkgs[p.name] = p
		items = append(items, rhunext.TestItem{ID: "cargo:" + p.name, Label: "Cargo: " + p.name, Kind: rhunext.TestGroup,
			Lazy: true, Path: wire(filepath.Join(dir, "Cargo.toml"))})
	}
	return items
}

// split: "cargo:PKG[:TARGET[:PATH]]"
func cargoSplit(id string) (pkg, target, path string) {
	rest := strings.TrimPrefix(id, "cargo:")
	parts := strings.SplitN(rest, ":", 3)
	pkg = parts[0]
	if len(parts) > 1 {
		target = parts[1]
	}
	if len(parts) > 2 {
		path = parts[2]
	}
	return
}

// targetFiles: the files of a target: the library's or binary's source tree, or one tests/ file
func (p *cargoPkg) targetFiles(target string) (root string, files []string) {
	switch {
	case target == "lib" || target == "bin":
		root = filepath.Join(p.dir, "src", "lib.rs")
		if target == "bin" {
			root = filepath.Join(p.dir, "src", "main.rs")
		}
		filepath.WalkDir(filepath.Join(p.dir, "src"), func(f string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && d.Name() == "bin" {
				return filepath.SkipDir
			}
			if !d.IsDir() && strings.HasSuffix(f, ".rs") {
				base := filepath.Base(f)
				if (target == "lib" && base == "main.rs" && filepath.Dir(f) == filepath.Join(p.dir, "src")) ||
					(target == "bin" && base == "lib.rs" && filepath.Dir(f) == filepath.Join(p.dir, "src")) {
					return nil
				}
				files = append(files, f)
			}
			return nil
		})
	case strings.HasPrefix(target, "tests/"):
		root = filepath.Join(p.dir, "tests", strings.TrimPrefix(target, "tests/")+".rs")
		files = []string{root}
	}
	sort.Strings(files)
	return
}

func (c *cargoAdapter) resolve(id string) ([]rhunext.TestItem, error) {
	pkgName, target, _ := cargoSplit(id)
	c.mu.Lock()
	p := c.pkgs[pkgName]
	c.mu.Unlock()
	if p == nil {
		return nil, &rhunext.Error{Code: "refused", Text: "unknown package"}
	}
	if target == "" {
		var items []rhunext.TestItem
		for _, t := range []string{"lib", "bin"} {
			root, files := p.targetFiles(t)
			if _, err := os.Stat(root); err == nil && len(rustTests(p, t, root, files)) > 0 {
				label := map[string]string{"lib": "lib", "bin": "bin"}[t]
				items = append(items, rhunext.TestItem{ID: "cargo:" + p.name + ":" + t, Label: label,
					Kind: rhunext.TestGroup, Lazy: true, Path: wire(root)})
			}
		}
		tests, _ := filepath.Glob(filepath.Join(p.dir, "tests", "*.rs"))
		sort.Strings(tests)
		for _, f := range tests {
			name := strings.TrimSuffix(filepath.Base(f), ".rs")
			items = append(items, rhunext.TestItem{ID: "cargo:" + p.name + ":tests/" + name, Label: "tests/" + name,
				Kind: rhunext.TestFile, Lazy: true, Path: wire(f)})
		}
		return items, nil
	}
	root, files := p.targetFiles(target)
	c.mu.Lock()
	c.resolved[id] = true
	c.mu.Unlock()
	return rustTests(p, target, root, files), nil
}

var reRustFn = regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?fn\s+([A-Za-z_][A-Za-z0-9_]*)`)
var reRustMod = regexp.MustCompile(`^\s*(?:pub(?:\([^)]*\))?\s+)?mod\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{`)
var reTestAttr = regexp.MustCompile(`^\s*#\[(?:test|tokio::test|async_std::test|rstest)(?:\(.*\))?\]`)

// rustTests: the #[test] functions of the target's files, with their module paths
func rustTests(p *cargoPkg, target, root string, files []string) []rhunext.TestItem {
	var items []rhunext.TestItem
	for _, f := range files {
		base := modulePath(p, target, root, f)
		src, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		lines := strings.Split(string(src), "\n")
		type mod struct {
			name  string
			depth int
		}
		var mods []mod
		depth := 0
		pending, ignored := false, false
		for i, line := range lines {
			code := line
			if c := strings.Index(code, "//"); c >= 0 {
				code = code[:c]
			}
			if m := reRustMod.FindStringSubmatch(code); m != nil {
				mods = append(mods, mod{m[1], depth})
			}
			if reTestAttr.MatchString(code) {
				pending = true
			} else if strings.Contains(code, "#[ignore") {
				ignored = true
			} else if pending {
				if m := reRustFn.FindStringSubmatch(code); m != nil {
					path := base
					for _, md := range mods {
						path = append(path, md.name)
					}
					path = append(path, m[1])
					full := strings.Join(path, "::")
					end := i + 1
					// the function's end: its closing brace at the indentation of its line (or this
					// line, for a body that opens and closes on it)
					indent := len(line) - len(strings.TrimLeft(line, " \t"))
					open, closed := strings.Count(code, "{"), strings.Count(code, "}")
					for j := i + 1; j < len(lines) && !(open > 0 && open == closed); j++ {
						t := strings.TrimRight(lines[j], " \t\r")
						if t == strings.Repeat(" ", indent)+"}" || t == strings.Repeat("\t", indent)+"}" {
							end = j + 1
							break
						}
					}
					endCol := indent + 2 // after the closing brace
					if end == i+1 {
						endCol = len(line) + 1
					}
					it := rhunext.TestItem{ID: "cargo:" + p.name + ":" + target + ":" + full, Label: full,
						Kind: rhunext.TestCase, Path: wire(f),
						Range: &rhunext.Range{Start: rhunext.Pos{Line: i + 1, Col: indent + 1}, End: rhunext.Pos{Line: end, Col: endCol}}}
					if ignored {
						it.Desc = "ignored"
					}
					items = append(items, it)
					pending, ignored = false, false
				} else if strings.TrimSpace(code) != "" && !strings.HasPrefix(strings.TrimSpace(code), "#[") {
					pending, ignored = false, false
				}
			}
			depth += strings.Count(code, "{") - strings.Count(code, "}")
			for len(mods) > 0 && depth <= mods[len(mods)-1].depth {
				mods = mods[:len(mods)-1]
			}
		}
	}
	return items
}

// modulePath: a file's module path within its target (src/a/b.rs is a::b, src/a/mod.rs is a)
func modulePath(p *cargoPkg, target, root, f string) []string {
	if f == root {
		return nil
	}
	rel, err := filepath.Rel(filepath.Join(p.dir, "src"), f)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil
	}
	rel = strings.TrimSuffix(filepath.ToSlash(rel), ".rs")
	rel = strings.TrimSuffix(rel, "/mod")
	return strings.Split(rel, "/")
}

func (c *cargoAdapter) refresh(path string) []string {
	if !strings.HasSuffix(path, ".rs") {
		return nil
	}
	path = filepath.Clean(path) // rhun's C:/w/x/src/lib.rs is C:\w\x\src\lib.rs like the package folders
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for id := range c.resolved {
		pkgName, target, _ := cargoSplit(id)
		if p := c.pkgs[pkgName]; p != nil && strings.HasPrefix(path, p.dir+string(filepath.Separator)) {
			_ = target
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// ---- runs

type cargoJob struct {
	pkg    *cargoPkg
	target string
	tests  []string // exact names, none: the whole target
}

func (c *cargoAdapter) jobs(ids []string) []cargoJob {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []cargoJob
	targetsOf := func(p *cargoPkg) []string {
		var ts []string
		for _, t := range []string{"lib", "bin"} {
			root, _ := p.targetFiles(t)
			if _, err := os.Stat(root); err == nil {
				ts = append(ts, t)
			}
		}
		tests, _ := filepath.Glob(filepath.Join(p.dir, "tests", "*.rs"))
		sort.Strings(tests)
		for _, f := range tests {
			ts = append(ts, "tests/"+strings.TrimSuffix(filepath.Base(f), ".rs"))
		}
		return ts
	}
	if len(ids) == 0 {
		var names []string
		for n := range c.pkgs {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			for _, t := range targetsOf(c.pkgs[n]) {
				out = append(out, cargoJob{pkg: c.pkgs[n], target: t})
			}
		}
		return out
	}
	idx := map[string]int{}
	whole := map[string]bool{}
	for _, id := range ids {
		pkgName, target, path := cargoSplit(id)
		p := c.pkgs[pkgName]
		if p == nil {
			continue
		}
		if target == "" {
			for _, t := range targetsOf(p) {
				out = append(out, cargoJob{pkg: p, target: t})
			}
			continue
		}
		key := pkgName + ":" + target
		i, ok := idx[key]
		if !ok {
			out = append(out, cargoJob{pkg: p, target: target})
			i = len(out) - 1
			idx[key] = i
		}
		if path == "" {
			whole[key] = true
		} else {
			out[i].tests = append(out[i].tests, path)
		}
	}
	for key, i := range idx {
		if whole[key] {
			out[i].tests = nil // the whole target: no exact names
		}
	}
	return out
}

// publish: a job's target and tests when nobody expanded them, so their results have somewhere to go
func (c *cargoAdapter) publish(j cargoJob, rep *rhunext.TestReporter) {
	id := "cargo:" + j.pkg.name + ":" + j.target
	c.mu.Lock()
	done := c.resolved[id]
	c.resolved[id] = true
	c.mu.Unlock()
	if done {
		return
	}
	root, files := j.pkg.targetFiles(j.target)
	kind := rhunext.TestGroup
	if strings.HasPrefix(j.target, "tests/") {
		kind = rhunext.TestFile
	}
	rep.AddItems("cargo:"+j.pkg.name, rhunext.TestItem{ID: id, Label: j.target, Kind: kind, Path: wire(root)})
	if tests := rustTests(j.pkg, j.target, root, files); len(tests) > 0 {
		rep.AddItems(id, tests...)
	}
}

func (c *cargoAdapter) run(x *ext, r rhunext.TestRun, ids []string, rep *rhunext.TestReporter) error {
	st := x.runState(r.Run)
	jobs := c.jobs(ids)
	for _, j := range jobs {
		c.publish(j, rep)
	}
	for n, j := range jobs {
		if st.isCancelled() {
			break
		}
		args := []string{"test", "--no-fail-fast", "-p", j.pkg.name}
		var lcov string
		if r.Profile == "coverage" {
			state := os.Getenv("RHUN_STATE")
			if state == "" {
				state = os.TempDir()
			}
			os.MkdirAll(filepath.Join(state, "cover"), 0o700)
			lcov = filepath.Join(state, "cover", fmt.Sprintf("run%d-cargo%d.lcov", r.Run, n))
			os.Remove(lcov)
			args = []string{"llvm-cov", "--lcov", "--output-path", lcov, "--no-fail-fast", "-p", j.pkg.name}
		}
		switch {
		case j.target == "lib":
			args = append(args, "--lib")
		case j.target == "bin":
			args = append(args, "--bins")
		case strings.HasPrefix(j.target, "tests/"):
			args = append(args, "--test", strings.TrimPrefix(j.target, "tests/"))
		}
		args = append(args, x.flags("cargo_flags")...)
		if len(j.tests) > 0 {
			args = append(args, "--", "--exact", "--include-ignored")
			args = append(args, j.tests...)
		}
		p, err := x.e.TestExec(r.Run, rhunext.TestCommand{Program: "cargo", Dir: wire(j.pkg.dir), Args: args})
		if err != nil {
			return fmt.Errorf("cargo test: %v", err)
		}
		missing := c.consume(j, p, rep)
		if lcov != "" {
			if missing {
				rep.Add(rhunext.TestResult{State: rhunext.TestErrored, Duration: rhunext.NoDuration, Messages: []rhunext.TestMessage{{
					Text: "Coverage for Rust needs cargo-llvm-cov (cargo install cargo-llvm-cov; rustup component add llvm-tools-preview)"}}})
				break
			}
			if files, err := readLcov(lcov); err == nil && len(files) > 0 {
				rep.Flush()
				x.e.ReportCoverage(r.Run, files)
			}
			os.Remove(lcov)
		}
	}
	return nil
}

var reLibtest = regexp.MustCompile(`^test (\S+) \.\.\. (ok|FAILED|ignored)`)
var rePanic = regexp.MustCompile(`panicked at ([^:]+):(\d+):(\d+):$`)
var reStdout = regexp.MustCompile(`^---- (\S+) stdout ----$`)
var reCompileLoc = regexp.MustCompile(`^\s*--> ([^:]+):(\d+):(\d+)`)

// consume reads cargo test's output until it ends; it returns true when cargo said the command
// (llvm-cov) does not exist
func (c *cargoAdapter) consume(j cargoJob, p *rhunext.TestProcess, rep *rhunext.TestReporter) bool {
	prefix := "cargo:" + j.pkg.name + ":" + j.target + ":"
	errs := make(chan string, 1)
	go func() {
		var b strings.Builder
		sc := bufio.NewScanner(p.Stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			b.WriteString(sc.Text())
			b.WriteByte('\n')
		}
		errs <- b.String()
	}()
	ran := false
	failed := map[string]bool{}
	var section string // the test whose stdout section is being read
	var out strings.Builder
	finish := func() {
		if section == "" {
			return
		}
		text := out.String()
		res := rhunext.TestResult{Item: prefix + section, Duration: rhunext.NoDuration, Output: []byte(text)}
		if failed[section] {
			res.State = rhunext.TestFailed
			res.Messages = panicMessages(text, j.pkg.dir)
		}
		rep.Add(res)
		section = ""
		out.Reset()
	}
	sc := bufio.NewScanner(p.Stdout)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "running ") {
			ran = true
		}
		if m := reStdout.FindStringSubmatch(line); m != nil {
			finish()
			section = m[1]
			continue
		}
		if section != "" {
			if line == "failures:" || strings.HasPrefix(line, "test result:") {
				finish()
			} else {
				out.WriteString(line)
				out.WriteByte('\n')
				continue
			}
		}
		if m := reLibtest.FindStringSubmatch(line); m != nil {
			id := prefix + m[1]
			switch m[2] {
			case "ok":
				rep.Add(rhunext.TestResult{Item: id, State: rhunext.TestPassed, Duration: rhunext.NoDuration})
			case "ignored":
				rep.Add(rhunext.TestResult{Item: id, State: rhunext.TestSkipped, Duration: rhunext.NoDuration})
			case "FAILED":
				failed[m[1]] = true
				// the message comes with its stdout section; failed now, in case there is none
				rep.Add(rhunext.TestResult{Item: id, State: rhunext.TestFailed, Duration: rhunext.NoDuration})
			}
		}
	}
	finish()
	code := p.Wait()
	stderr := <-errs
	if strings.Contains(stderr, "no such command") {
		return true
	}
	if !ran && code != "0" {
		// it did not build: the compiler's errors on the target's item
		rep.Add(rhunext.TestResult{Item: strings.TrimSuffix(prefix, ":"), State: rhunext.TestErrored,
			Duration: rhunext.NoDuration, Messages: compileMessages(stderr, j.pkg.dir), Output: []byte(stderr)})
	}
	return false
}

// panicMessages: "panicked at FILE:L:C:" and the lines after it, with assert_eq!'s values
func panicMessages(text, dir string) []rhunext.TestMessage {
	lines := strings.Split(text, "\n")
	var msgs []rhunext.TestMessage
	for i, line := range lines {
		m := rePanic.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		var body []string
		for _, l := range lines[i+1:] {
			if l == "" || strings.HasPrefix(l, "note: ") || strings.HasPrefix(l, "thread '") {
				break
			}
			body = append(body, l)
		}
		ln, _ := strconv.Atoi(m[2])
		col, _ := strconv.Atoi(m[3])
		msg := rhunext.TestMessage{Text: strings.Join(body, "\n"), Path: wire(findFile(dir, m[1])),
			Range: &rhunext.Range{Start: rhunext.Pos{Line: ln, Col: col}, End: rhunext.Pos{Line: ln, Col: col}}}
		if msg.Text == "" {
			msg.Text = "panicked"
		}
		if exp, act, ok := rhunext.ExpectedActual(msg.Text); ok {
			e, a := exp, act
			msg.Expected, msg.Actual = &e, &a
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

// compileMessages: rustc's "error..." lines with their "--> FILE:L:C" places
func compileMessages(stderr, dir string) []rhunext.TestMessage {
	lines := strings.Split(stderr, "\n")
	var msgs []rhunext.TestMessage
	for i, line := range lines {
		if !strings.HasPrefix(line, "error") || strings.HasPrefix(line, "error: could not compile") {
			continue
		}
		msg := rhunext.TestMessage{Text: line}
		if i+1 < len(lines) {
			if m := reCompileLoc.FindStringSubmatch(lines[i+1]); m != nil {
				ln, _ := strconv.Atoi(m[2])
				col, _ := strconv.Atoi(m[3])
				msg.Path = wire(findFile(dir, m[1]))
				msg.Range = &rhunext.Range{Start: rhunext.Pos{Line: ln, Col: col}, End: rhunext.Pos{Line: ln, Col: col}}
			}
		}
		msgs = append(msgs, msg)
		if len(msgs) == 16 {
			break
		}
	}
	if len(msgs) == 0 {
		msgs = []rhunext.TestMessage{{Text: "cargo test failed before running tests"}}
	}
	return msgs
}

// findFile: a path cargo printed, relative to the package or a workspace above it
func findFile(dir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	for d := dir; ; {
		f := filepath.Join(d, p)
		if _, err := os.Stat(f); err == nil {
			return f
		}
		up := filepath.Dir(d)
		if up == d {
			return filepath.Join(dir, p)
		}
		d = up
	}
}

// readLcov: SF: and DA:LINE,COUNT records
func readLcov(path string) ([]rhunext.FileCoverage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []rhunext.FileCoverage
	var cur *rhunext.FileCoverage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "SF:"):
			out = append(out, rhunext.FileCoverage{Path: wire(strings.TrimPrefix(line, "SF:"))})
			cur = &out[len(out)-1]
		case strings.HasPrefix(line, "DA:") && cur != nil:
			parts := strings.Split(strings.TrimPrefix(line, "DA:"), ",")
			if len(parts) >= 2 {
				ln, _ := strconv.Atoi(parts[0])
				n, _ := strconv.ParseInt(parts[1], 10, 64)
				if ln > 0 {
					cur.Ranges = append(cur.Ranges, rhunext.CoverageRange{From: ln, To: ln, Count: n})
				}
			}
		case line == "end_of_record":
			cur = nil
		}
	}
	return out, sc.Err()
}
