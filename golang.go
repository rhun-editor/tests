package main

// The Go adapter. Items: a module (gomod:MODULE), its packages with tests (go:IMPORTPATH), their
// tests (go:IMPORTPATH.TestName), and subtests found while running (go:IMPORTPATH.TestName/case).
// Tests come from reading the _test.go files (TestXxx, BenchmarkXxx, FuzzXxx, ExampleXxx with an
// output comment); a run is one `go test -json` per module or package, its events mapped to results.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

type goModule struct {
	path string // the module path
	dir  string
}

type goAdapter struct {
	mu       sync.Mutex
	modules  map[string]*goModule // by module path
	pkgDirs  map[string]string    // import path -> folder, for the packages listed so far
	resolved map[string]bool      // packages whose tests were given
}

func newGoAdapter() *goAdapter {
	return &goAdapter{modules: map[string]*goModule{}, pkgDirs: map[string]string{}, resolved: map[string]bool{}}
}

func (g *goAdapter) owns(id string) bool {
	return strings.HasPrefix(id, "go:") || strings.HasPrefix(id, "gomod:")
}

var reModule = regexp.MustCompile(`(?m)^\s*module\s+("?)([^"\s]+)("?)\s*$`)

func (g *goAdapter) roots(project string) []rhunext.TestItem {
	var items []rhunext.TestItem
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, dir := range walk(project, "go.mod", 200) {
		src, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err != nil {
			continue
		}
		m := reModule.FindSubmatch(src)
		if m == nil {
			continue
		}
		mod := &goModule{path: string(m[2]), dir: dir}
		g.modules[mod.path] = mod
		items = append(items, rhunext.TestItem{ID: "gomod:" + mod.path, Label: "Go: " + mod.path,
			Kind: rhunext.TestGroup, Lazy: true, Path: filepath.Join(dir, "go.mod")})
	}
	return items
}

func (g *goAdapter) module(path string) *goModule {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.modules[path]
}

// packages: the module's folders with _test.go files (not inside a nested module), as import paths
func (g *goAdapter) packages(mod *goModule) map[string]string {
	out := map[string]string{}
	filepath.WalkDir(mod.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			b := d.Name()
			if p != mod.dir {
				if strings.HasPrefix(b, ".") || strings.HasPrefix(b, "_") || b == "vendor" || b == "testdata" || b == "node_modules" {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") {
			dir := filepath.Dir(p)
			rel, _ := filepath.Rel(mod.dir, dir)
			imp := mod.path
			if rel != "." {
				imp += "/" + filepath.ToSlash(rel)
			}
			out[imp] = dir
		}
		return nil
	})
	return out
}

func (g *goAdapter) resolve(id string) ([]rhunext.TestItem, error) {
	if strings.HasPrefix(id, "gomod:") {
		mod := g.module(strings.TrimPrefix(id, "gomod:"))
		if mod == nil {
			return nil, &rhunext.Error{Code: "refused", Text: "unknown module"}
		}
		pkgs := g.packages(mod)
		var names []string
		for imp := range pkgs {
			names = append(names, imp)
		}
		sort.Strings(names)
		var items []rhunext.TestItem
		g.mu.Lock()
		for _, imp := range names {
			g.pkgDirs[imp] = pkgs[imp]
			label := strings.TrimPrefix(strings.TrimPrefix(imp, mod.path), "/")
			if label == "" {
				label = filepath.Base(mod.path)
			}
			items = append(items, rhunext.TestItem{ID: "go:" + imp, Label: label, Kind: rhunext.TestGroup,
				Lazy: true, Desc: imp})
		}
		g.mu.Unlock()
		return items, nil
	}
	imp := strings.TrimPrefix(id, "go:")
	g.mu.Lock()
	dir, ok := g.pkgDirs[imp]
	g.mu.Unlock()
	if !ok {
		return nil, &rhunext.Error{Code: "refused", Text: "unknown package"}
	}
	tests, err := goTests(dir, imp)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.resolved[imp] = true
	g.mu.Unlock()
	return tests, nil
}

// goTests: the package's tests, in file order
func goTests(dir, imp string) ([]rhunext.TestItem, error) {
	files, _ := filepath.Glob(filepath.Join(dir, "*_test.go"))
	sort.Strings(files)
	var items []rhunext.TestItem
	fset := token.NewFileSet()
	for _, f := range files {
		file, err := parser.ParseFile(fset, f, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil && file == nil {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			name := fn.Name.Name
			kind, desc := testKind(name, fn, file)
			if kind == "" {
				continue
			}
			start, end := fset.Position(fn.Pos()), fset.Position(fn.End())
			items = append(items, rhunext.TestItem{ID: "go:" + imp + "." + name, Label: name, Kind: rhunext.TestCase,
				Path: f, Range: &rhunext.Range{Start: rhunext.Pos{Line: start.Line, Col: start.Column},
					End: rhunext.Pos{Line: end.Line, Col: end.Column}}, Desc: desc, NoDebug: kind == "example"})
		}
	}
	return items, nil
}

// testKind: "test", "benchmark", "fuzz" or "example" for a function go test runs, "" otherwise
func testKind(name string, fn *ast.FuncDecl, file *ast.File) (string, string) {
	after := func(prefix string) bool {
		if !strings.HasPrefix(name, prefix) {
			return false
		}
		rest := name[len(prefix):]
		if rest == "" {
			return true
		}
		r, _ := utf8.DecodeRuneInString(rest)
		return !unicode.IsLower(r)
	}
	params := fn.Type.Params
	switch {
	case after("Test") && params != nil && len(params.List) == 1:
		return "test", ""
	case after("Benchmark") && params != nil && len(params.List) == 1:
		return "benchmark", "benchmark"
	case after("Fuzz") && params != nil && len(params.List) == 1:
		return "fuzz", "fuzz seeds"
	case after("Example") && (params == nil || len(params.List) == 0) && fn.Body != nil && hasOutputComment(fn, file):
		return "example", "example"
	}
	return "", ""
}

// hasOutputComment: an example runs only with an "Output:" comment in its body
func hasOutputComment(fn *ast.FuncDecl, file *ast.File) bool {
	for _, cg := range file.Comments {
		if cg.Pos() < fn.Body.Lbrace || cg.End() > fn.Body.Rbrace {
			continue
		}
		t := strings.TrimSpace(cg.Text())
		if strings.HasPrefix(t, "Output:") || strings.HasPrefix(t, "Unordered output:") {
			return true
		}
	}
	return false
}

func (g *goAdapter) refresh(path string) []string {
	if !strings.HasSuffix(path, "_test.go") {
		return nil
	}
	dir := filepath.Dir(path)
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for imp, d := range g.pkgDirs {
		if d == dir && g.resolved[imp] {
			out = append(out, "go:"+imp)
		}
	}
	return out
}

// ---- runs

// goJob is one go test command: in a module's folder, of packages, with a -run pattern
type goJob struct {
	mod     *goModule
	pkgs    []string // ./... or ./rel
	pattern string   // -run, "" for all
	bench   string   // -bench, "" for none
}

// jobs: what the ids ask for, one job per package (or module, or the whole module for nothing)
func (g *goAdapter) jobs(ids []string) []goJob {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []goJob
	if len(ids) == 0 {
		var mods []string
		for p := range g.modules {
			mods = append(mods, p)
		}
		sort.Strings(mods)
		for _, p := range mods {
			out = append(out, goJob{mod: g.modules[p], pkgs: []string{"./..."}})
		}
		return out
	}
	type pkgSel struct {
		tests []string // top-level tests, nil: all
		all   bool
	}
	sel := map[string]*pkgSel{}
	var order []string
	for _, id := range ids {
		if strings.HasPrefix(id, "gomod:") {
			if m := g.modules[strings.TrimPrefix(id, "gomod:")]; m != nil {
				out = append(out, goJob{mod: m, pkgs: []string{"./..."}})
			}
			continue
		}
		rest := strings.TrimPrefix(id, "go:")
		imp, test := rest, ""
		if _, ok := g.pkgDirs[rest]; !ok {
			// a test: the package is the longest known prefix before a dot
			for i := len(rest) - 1; i > 0; i-- {
				if rest[i] == '.' {
					if _, ok := g.pkgDirs[rest[:i]]; ok {
						imp, test = rest[:i], rest[i+1:]
						break
					}
				}
			}
		}
		if _, ok := g.pkgDirs[imp]; !ok {
			continue
		}
		s := sel[imp]
		if s == nil {
			s = &pkgSel{}
			sel[imp] = s
			order = append(order, imp)
		}
		if test == "" {
			s.all = true
		} else if strings.Contains(test, "/") {
			// a subtest: a job of its own, its levels each anchored
			parts := strings.Split(test, "/")
			for i, p := range parts {
				parts[i] = "^" + regexp.QuoteMeta(p) + "$"
			}
			out = append(out, g.pkgJob(imp, strings.Join(parts, "/"), ""))
		} else {
			s.tests = append(s.tests, test)
		}
	}
	for _, imp := range order {
		s := sel[imp]
		if s.all {
			out = append(out, g.pkgJob(imp, "", ""))
			continue
		}
		if len(s.tests) == 0 {
			continue
		}
		var tests, benches []string
		for _, t := range s.tests {
			if strings.HasPrefix(t, "Benchmark") {
				benches = append(benches, regexp.QuoteMeta(t))
			} else {
				tests = append(tests, regexp.QuoteMeta(t))
			}
		}
		if len(tests) > 0 {
			out = append(out, g.pkgJob(imp, "^("+strings.Join(tests, "|")+")$", ""))
		}
		if len(benches) > 0 {
			out = append(out, g.pkgJob(imp, "^$", "^("+strings.Join(benches, "|")+")$"))
		}
	}
	return out
}

// pkgJob: a job for one package (the lock is held)
func (g *goAdapter) pkgJob(imp, pattern, bench string) goJob {
	dir := g.pkgDirs[imp]
	for _, m := range g.modules {
		if imp == m.path || strings.HasPrefix(imp, m.path+"/") {
			rel, _ := filepath.Rel(m.dir, dir)
			return goJob{mod: m, pkgs: []string{"./" + filepath.ToSlash(rel)}, pattern: pattern, bench: bench}
		}
	}
	return goJob{}
}

// publish: the items a job reports on that were not given yet (nobody expanded them), so their
// results have somewhere to go
func (g *goAdapter) publish(j goJob, rep *rhunext.TestReporter) {
	var pkgs map[string]string
	if len(j.pkgs) == 1 && j.pkgs[0] == "./..." {
		pkgs = g.packages(j.mod)
	} else {
		pkgs = map[string]string{}
		g.mu.Lock()
		for _, p := range j.pkgs {
			dir := filepath.Join(j.mod.dir, filepath.FromSlash(strings.TrimPrefix(p, "./")))
			for imp, d := range g.pkgDirs {
				if d == dir {
					pkgs[imp] = d
				}
			}
		}
		g.mu.Unlock()
	}
	var names []string
	for imp := range pkgs {
		names = append(names, imp)
	}
	sort.Strings(names)
	for _, imp := range names {
		g.mu.Lock()
		_, listed := g.pkgDirs[imp]
		done := g.resolved[imp]
		g.pkgDirs[imp] = pkgs[imp]
		g.mu.Unlock()
		if done {
			continue
		}
		if !listed {
			label := strings.TrimPrefix(strings.TrimPrefix(imp, j.mod.path), "/")
			if label == "" {
				label = filepath.Base(j.mod.path)
			}
			rep.AddItems("gomod:"+j.mod.path, rhunext.TestItem{ID: "go:" + imp, Label: label, Kind: rhunext.TestGroup, Desc: imp})
		}
		if tests, err := goTests(pkgs[imp], imp); err == nil && len(tests) > 0 {
			rep.AddItems("go:"+imp, tests...)
		}
		g.mu.Lock()
		g.resolved[imp] = true
		g.mu.Unlock()
	}
}

func (g *goAdapter) run(x *ext, r rhunext.TestRun, ids []string, rep *rhunext.TestReporter) error {
	jobs := g.jobs(ids)
	st := x.runState(r.Run)
	for _, j := range jobs {
		if j.mod != nil {
			g.publish(j, rep)
		}
	}
	for n, j := range jobs {
		if j.mod == nil || st.isCancelled() {
			continue
		}
		args := []string{"test", "-json"}
		if j.pattern != "" {
			args = append(args, "-run", j.pattern)
		}
		if j.bench != "" {
			args = append(args, "-bench", j.bench)
		}
		var profile string
		if r.Profile == "coverage" {
			state := os.Getenv("RHUN_STATE")
			if state == "" {
				state = os.TempDir()
			}
			os.MkdirAll(filepath.Join(state, "cover"), 0o700)
			profile = filepath.Join(state, "cover", fmt.Sprintf("run%d-%d.out", r.Run, n))
			os.Remove(profile)
			args = append(args, "-coverprofile="+profile, "-covermode=count")
		}
		args = append(args, x.flags("go_flags")...)
		args = append(args, j.pkgs...)
		p, err := x.e.TestExec(r.Run, rhunext.TestCommand{Program: "go", Dir: j.mod.dir, Args: args})
		if err != nil {
			return fmt.Errorf("go test: %v", err)
		}
		g.consume(x, j, p, rep)
		if profile != "" {
			if files, err := goCoverage(profile, g.moduleList()); err == nil && len(files) > 0 {
				rep.Flush() // results first, then the coverage
				x.e.ReportCoverage(r.Run, files)
			}
			os.Remove(profile)
		}
	}
	return nil
}

func (g *goAdapter) moduleList() []*goModule {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []*goModule
	for _, m := range g.modules {
		out = append(out, m)
	}
	return out
}

// goEvent is one line of go test -json (test2json)
type goEvent struct {
	Action      string
	Package     string
	ImportPath  string
	Test        string
	Elapsed     float64
	Output      string
	FailedBuild string
}

// goTest is what a test's output said so far
type goTest struct {
	output   strings.Builder
	messages []rhunext.TestMessage
	cur      *rhunext.TestMessage
}

var reGoLoc = regexp.MustCompile(`^\s+([^\s:]+\.go):(\d+): ?(.*)$`)
var reBuildLoc = regexp.MustCompile(`^([^\s:]+\.go):(\d+):(\d+): (.*)$`)

// consume reads the process's events until it ends, reporting results
func (g *goAdapter) consume(x *ext, j goJob, p *rhunext.TestProcess, rep *rhunext.TestReporter) {
	stderr := make(chan string, 1)
	go func() {
		var b strings.Builder
		sc := bufio.NewScanner(p.Stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			b.WriteString(sc.Text())
			b.WriteByte('\n')
		}
		stderr <- b.String()
	}()
	tests := map[string]*goTest{}
	known := map[string]bool{}
	builds := map[string]*strings.Builder{} // import path -> build output
	sc := bufio.NewScanner(p.Stdout)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var ev goEvent
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &ev) != nil {
			rep.Add(rhunext.TestResult{Duration: rhunext.NoDuration, Output: append(append([]byte(nil), line...), '\n')})
			continue
		}
		switch ev.Action {
		case "build-output":
			imp := strings.Fields(ev.ImportPath + " ")[0]
			b := builds[imp]
			if b == nil {
				b = &strings.Builder{}
				builds[imp] = b
			}
			b.WriteString(ev.Output)
			continue
		case "build-fail":
			continue
		}
		pkgID := "go:" + ev.Package
		if ev.Test == "" {
			switch ev.Action {
			case "output":
				rep.Add(rhunext.TestResult{Item: pkgID, Duration: rhunext.NoDuration, Output: []byte(ev.Output)})
			case "fail":
				if ev.FailedBuild != "" {
					imp := strings.Fields(ev.FailedBuild + " ")[0]
					var msgs []rhunext.TestMessage
					out := ""
					if b := builds[imp]; b != nil {
						out = b.String()
						msgs = buildMessages(out, j.mod.dir)
					}
					if len(msgs) == 0 {
						msgs = []rhunext.TestMessage{{Text: "build failed"}}
					}
					rep.Add(rhunext.TestResult{Item: pkgID, State: rhunext.TestErrored, Duration: rhunext.NoDuration,
						Messages: msgs, Output: []byte(out)})
				} else {
					rep.Add(rhunext.TestResult{Item: pkgID, State: rhunext.TestFailed, Duration: ms(ev.Elapsed)})
				}
			case "pass":
				rep.Add(rhunext.TestResult{Item: pkgID, State: rhunext.TestPassed, Duration: ms(ev.Elapsed)})
			case "skip":
				rep.Add(rhunext.TestResult{Item: pkgID, State: rhunext.TestSkipped, Duration: ms(ev.Elapsed)})
			}
			continue
		}
		id := pkgID + "." + ev.Test
		t := tests[id]
		if t == nil {
			t = &goTest{}
			tests[id] = t
		}
		switch ev.Action {
		case "run":
			if strings.Contains(ev.Test, "/") && !known[id] {
				// a subtest found while running: an item under its parent
				parent := id[:strings.LastIndexByte(id, '/')]
				rep.AddItems(parent, rhunext.TestItem{ID: id, Label: ev.Test[strings.LastIndexByte(ev.Test, '/')+1:],
					Kind: rhunext.TestCase})
			}
			known[id] = true
			rep.Add(rhunext.TestResult{Item: id, State: rhunext.TestStarted, Duration: rhunext.NoDuration})
		case "output":
			t.output.WriteString(ev.Output)
			g.messageLine(t, ev.Output, ev.Package)
		case "pass", "fail", "skip", "bench":
			state := rhunext.TestPassed
			switch ev.Action {
			case "fail":
				state = rhunext.TestFailed
			case "skip":
				state = rhunext.TestSkipped
			}
			res := rhunext.TestResult{Item: id, State: state, Duration: ms(ev.Elapsed), Output: []byte(t.output.String())}
			if state == rhunext.TestFailed {
				res.Messages = finishMessages(t)
			}
			rep.Add(res)
			t.output.Reset()
		}
	}
	code := p.Wait()
	if errs := <-stderr; errs != "" {
		rep.Add(rhunext.TestResult{Duration: rhunext.NoDuration, Output: []byte(errs)})
	}
	if code != "0" && code != "1" && code != "2" && code != "killed" {
		rep.Add(rhunext.TestResult{State: rhunext.TestErrored, Duration: rhunext.NoDuration,
			Messages: []rhunext.TestMessage{{Text: "go test ended: " + code}}})
	}
}

// messageLine: a "    file_test.go:20: text" line starts a message; deeper lines continue it
func (g *goAdapter) messageLine(t *goTest, out, pkg string) {
	line := strings.TrimRight(out, "\n")
	if strings.HasPrefix(line, "=== ") || strings.HasPrefix(line, "--- ") {
		t.cur = nil
		return
	}
	if m := reGoLoc.FindStringSubmatch(line); m != nil && strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "        ") {
		g.mu.Lock()
		dir := g.pkgDirs[pkg]
		g.mu.Unlock()
		n, _ := strconv.Atoi(m[2])
		msg := rhunext.TestMessage{Text: m[3]}
		if dir != "" && n > 0 {
			msg.Path = filepath.Join(dir, m[1])
			msg.Range = &rhunext.Range{Start: rhunext.Pos{Line: n, Col: 1}, End: rhunext.Pos{Line: n, Col: 1}}
		}
		t.messages = append(t.messages, msg)
		t.cur = &t.messages[len(t.messages)-1]
		return
	}
	if t.cur != nil && strings.HasPrefix(line, "        ") {
		t.cur.Text += "\n" + strings.TrimPrefix(line, "        ")
	}
}

// finishMessages: the test's messages with their expected and actual values
func finishMessages(t *goTest) []rhunext.TestMessage {
	msgs := t.messages
	t.messages, t.cur = nil, nil
	for i := range msgs {
		if exp, act, ok := rhunext.ExpectedActual(msgs[i].Text); ok {
			e, a := exp, act
			msgs[i].Expected, msgs[i].Actual = &e, &a
		}
	}
	return msgs
}

// buildMessages: the compiler's "file.go:3:23: text" lines (relative to the module folder)
func buildMessages(out, dir string) []rhunext.TestMessage {
	var msgs []rhunext.TestMessage
	for _, line := range strings.Split(out, "\n") {
		m := reBuildLoc.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		l, _ := strconv.Atoi(m[2])
		c, _ := strconv.Atoi(m[3])
		p := m[1]
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		msgs = append(msgs, rhunext.TestMessage{Text: m[4], Path: p,
			Range: &rhunext.Range{Start: rhunext.Pos{Line: l, Col: c}, End: rhunext.Pos{Line: l, Col: c}}})
		if len(msgs) == 16 {
			break
		}
	}
	return msgs
}

func ms(seconds float64) time.Duration {
	return time.Duration(seconds * float64(time.Second))
}

// goCoverage reads a cover profile ("mode: count", then "IMPORTPATH/file.go:L1.C1,L2.C2 N COUNT")
// into files of the modules
func goCoverage(path string, mods []*goModule) ([]rhunext.FileCoverage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	byFile := map[string]*rhunext.FileCoverage{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") {
			continue
		}
		colon := strings.LastIndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name, rest := line[:colon], line[colon+1:]
		var l1, c1, l2, c2, n int
		var count int64
		if _, err := fmt.Sscanf(rest, "%d.%d,%d.%d %d %d", &l1, &c1, &l2, &c2, &n, &count); err != nil {
			continue
		}
		file := ""
		for _, m := range mods {
			if strings.HasPrefix(name, m.path+"/") {
				file = filepath.Join(m.dir, filepath.FromSlash(strings.TrimPrefix(name, m.path+"/")))
				break
			}
		}
		if file == "" {
			continue
		}
		fc := byFile[file]
		if fc == nil {
			fc = &rhunext.FileCoverage{Path: file}
			byFile[file] = fc
			order = append(order, file)
		}
		fc.Ranges = append(fc.Ranges, rhunext.CoverageRange{From: l1, To: l2, Count: count})
	}
	var out []rhunext.FileCoverage
	for _, f := range order {
		out = append(out, *byFile[f])
	}
	return out, sc.Err()
}
