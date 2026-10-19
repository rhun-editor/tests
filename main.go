// tests: Go and Rust tests in rhun's Testing view (protocol 1.1, feature tests). The project's Go
// modules (go.mod) and Cargo packages (Cargo.toml) are the roots; their packages and test targets
// are listed when expanded, found by reading the sources (go/parser, a scan for #[test]); a run asks
// rhun to start `go test -json` or `cargo test` as the user (test-exec) and turns their output into
// results, failure messages with expected and actual values, and coverage.
// Protocol: https://rhun.app/extensions/v1/
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

// adapter is one kind of project: its roots, their children, and runs of its items.
type adapter interface {
	// roots are the adapter's top items in the project (all lazy)
	roots(project string) []rhunext.TestItem
	// owns tells whether an id is the adapter's
	owns(id string) bool
	// resolve lists an item's children
	resolve(id string) ([]rhunext.TestItem, error)
	// run runs the items (none: all of the adapter's) and reports them; it returns when done
	run(x *ext, r rhunext.TestRun, ids []string, rep *rhunext.TestReporter) error
	// refresh: a file was saved: the ids of resolved items whose children may have changed
	refresh(path string) []string
}

type ext struct {
	e        *rhunext.Ext
	project  string
	settings rhunext.Settings
	adapters []adapter
	mu       sync.Mutex
	runs     map[int64]*runState
	paths    map[rhunext.Doc]string // open documents' files
}

type runState struct {
	mu        sync.Mutex
	cancelled bool
}

func main() {
	x := &ext{runs: map[int64]*runState{}}
	x.adapters = []adapter{newGoAdapter(), newCargoAdapter()}
	if err := rhunext.RunWith([]string{rhunext.FeatureTests}, x); err != nil && err.Error() != "EOF" {
		fmt.Fprintln(os.Stderr, "tests:", err)
	}
}

func (x *ext) Ready(e *rhunext.Ext, project string) {
	x.e = e
	x.project = project
}

func (x *ext) Run(e *rhunext.Ext, command string, doc rhunext.Doc) {}

func (x *ext) Event(e *rhunext.Ext, ev rhunext.Event) {
	if x.settings.Update(ev) {
		return
	}
	if ev.Name == "saved" {
		// a saved test file: the items it may change are listed again
		d, ok := rhunext.ParseDoc(ev)
		if !ok {
			return
		}
		path := x.docPath(d)
		if path == "" {
			return
		}
		for _, a := range x.adapters {
			for _, id := range a.refresh(path) {
				if items, err := a.resolve(id); err == nil {
					e.SetTestItems(id, items, true)
				}
			}
		}
	}
	if ev.Name == "opened" {
		if o, ok := rhunext.ParseOpened(ev); ok {
			x.mu.Lock()
			if x.paths == nil {
				x.paths = map[rhunext.Doc]string{}
			}
			x.paths[o.Doc] = o.Path
			x.mu.Unlock()
		}
	}
}

func (x *ext) docPath(d rhunext.Doc) string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.paths[d]
}

// ResolveTests: the roots, or an item's children
func (x *ext) ResolveTests(e *rhunext.Ext, parent string) ([]rhunext.TestItem, error) {
	if parent == "" {
		var items []rhunext.TestItem
		if x.project == "" {
			return nil, nil
		}
		for _, a := range x.adapters {
			items = append(items, a.roots(x.project)...)
		}
		return items, nil
	}
	for _, a := range x.adapters {
		if a.owns(parent) {
			return a.resolve(parent)
		}
	}
	return nil, &rhunext.Error{Code: "refused", Text: "unknown item"}
}

// RunTests starts the run on a goroutine: each adapter runs its share of the items in turn
func (x *ext) RunTests(e *rhunext.Ext, r rhunext.TestRun) error {
	if r.Profile == "debug" {
		return &rhunext.Error{Code: "refused", Text: "debugging tests needs rhun's debugger (not yet)"}
	}
	st := &runState{}
	x.mu.Lock()
	x.runs[r.Run] = st
	x.mu.Unlock()
	go func() {
		rep := rhunext.NewTestReporter(e, r.Run)
		for _, a := range x.adapters {
			var ids []string
			for _, id := range r.Items {
				if a.owns(id) {
					ids = append(ids, id)
				}
			}
			if len(r.Items) > 0 && len(ids) == 0 {
				continue
			}
			if st.isCancelled() {
				break
			}
			if err := a.run(x, r, ids, rep); err != nil {
				rep.Add(rhunext.TestResult{State: rhunext.TestErrored, Duration: rhunext.NoDuration,
					Messages: []rhunext.TestMessage{{Text: err.Error()}}})
			}
		}
		rep.Close()
		e.EndTestRun(r.Run)
		x.mu.Lock()
		delete(x.runs, r.Run)
		x.mu.Unlock()
	}()
	return nil
}

// CancelTests: rhun stops the run's processes itself; the adapters see the run cancelled and stop
func (x *ext) CancelTests(e *rhunext.Ext, run int64) {
	x.mu.Lock()
	st := x.runs[run]
	x.mu.Unlock()
	if st != nil {
		st.mu.Lock()
		st.cancelled = true
		st.mu.Unlock()
	}
}

func (s *runState) isCancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelled
}

func (x *ext) runState(run int64) *runState {
	x.mu.Lock()
	defer x.mu.Unlock()
	if s := x.runs[run]; s != nil {
		return s
	}
	return &runState{}
}

// flags is a setting's words
func (x *ext) flags(key string) []string {
	return strings.Fields(x.settings.String(key, ""))
}

// walk lists the folders under root holding a file named name, not going into skipped folders,
// in sorted order, at most limit
func walk(root, name string, limit int) []string {
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			b := d.Name()
			if p != root && (strings.HasPrefix(b, ".") || b == "vendor" || b == "node_modules" || b == "target" || b == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == name && len(out) < limit {
			out = append(out, filepath.Dir(p))
		}
		return nil
	})
	sort.Strings(out)
	return out
}
