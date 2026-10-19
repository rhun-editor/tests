package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rhun-editor/extension-protocol/go/rhunext"
)

func TestGoTestsFromSources(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a_test.go"), []byte("package a\n\nimport \"testing\"\n\n"+
		"func TestA(t *testing.T) {}\nfunc Testlower(t *testing.T) {}\nfunc BenchmarkB(b *testing.B) {}\n"+
		"func ExampleC() {\n\t// Output: x\n}\nfunc ExampleNoOutput() {}\nfunc helper() {}\n"), 0o644)
	items, err := goTests(dir, "ex.com/a")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, it := range items {
		names = append(names, it.Label+"="+it.Desc)
	}
	if got := strings.Join(names, " "); got != "TestA= BenchmarkB=benchmark ExampleC=example" {
		t.Fatal(got)
	}
	if items[0].Range.Start != (rhunext.Pos{Line: 5, Col: 1}) || items[0].ID != "go:ex.com/a.TestA" {
		t.Fatal(items[0])
	}
}

func TestGoMessagesAndValues(t *testing.T) {
	g := newGoAdapter()
	g.pkgDirs["ex.com/a"] = "/p/a"
	tt := &goTest{}
	for _, l := range []string{"=== RUN   TestG\n", "    a_test.go:20: Greet mismatch\n", "        got:  \"Hello\"\n",
		"        want: \"Hello!\\nWelcome.\"\n", "--- FAIL: TestG (0.00s)\n"} {
		g.messageLine(tt, l, "ex.com/a")
	}
	msgs := finishMessages(tt)
	if len(msgs) != 1 || msgs[0].Path != "/p/a/a_test.go" || msgs[0].Range.Start.Line != 20 ||
		*msgs[0].Expected != "Hello!\nWelcome." || *msgs[0].Actual != "Hello" {
		t.Fatalf("%+v", msgs)
	}
	b := buildMessages("# ex.com/a\nbad/bad.go:3:23: undefined: x\n", "/m")
	if len(b) != 1 || b[0].Path != "/m/bad/bad.go" || b[0].Range.Start != (rhunext.Pos{Line: 3, Col: 23}) {
		t.Fatalf("%+v", b)
	}
}

func TestGoCoverageProfile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "c.out")
	os.WriteFile(f, []byte("mode: count\nex.com/m/calc/calc.go:3.24,4.11 1 2\nex.com/m/calc/calc.go:4.11,6.3 1 0\nother.com/x.go:1.1,2.2 1 1\n"), 0o644)
	files, err := goCoverage(f, []*goModule{{path: "ex.com/m", dir: "/src/m"}})
	if err != nil || len(files) != 1 || files[0].Path != "/src/m/calc/calc.go" || len(files[0].Ranges) != 2 ||
		files[0].Ranges[1] != (rhunext.CoverageRange{From: 4, To: 6, Count: 0}) {
		t.Fatalf("%+v %v", files, err)
	}
}

func TestRustTestsAndPanics(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src", "net"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "lib.rs"), []byte("mod net;\n#[cfg(test)]\nmod tests {\n    #[test]\n    fn a() {\n        assert!(true);\n    }\n\n    #[test]\n    #[ignore]\n    fn b() {}\n}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "net", "mod.rs"), []byte("#[test]\nfn c() {}\n"), 0o644)
	p := &cargoPkg{name: "x", dir: dir}
	root, files := p.targetFiles("lib")
	items := rustTests(p, "lib", root, files)
	var got []string
	for _, it := range items {
		got = append(got, it.ID+"/"+it.Desc)
	}
	if strings.Join(got, " ") != "cargo:x:lib:tests::a/ cargo:x:lib:tests::b/ignored cargo:x:lib:net::c/" {
		t.Fatal(got)
	}
	if items[0].Range.End.Line != 7 || items[1].Range.End.Line != 11 {
		t.Fatal(items[0].Range, items[1].Range)
	}
	msgs := panicMessages("\nthread 't' panicked at src/lib.rs:16:9:\nassertion `left == right` failed: two\n  left: 4\n right: 5\nnote: run with\n", dir)
	if len(msgs) != 1 || msgs[0].Range.Start != (rhunext.Pos{Line: 16, Col: 9}) || *msgs[0].Expected != "5" || *msgs[0].Actual != "4" {
		t.Fatalf("%+v", msgs)
	}
}

func TestCargoJobs(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	os.MkdirAll(filepath.Join(dir, "tests"), 0o755)
	os.WriteFile(filepath.Join(dir, "src", "lib.rs"), []byte(""), 0o644)
	os.WriteFile(filepath.Join(dir, "tests", "api.rs"), []byte(""), 0o644)
	c := newCargoAdapter()
	c.pkgs["x"] = &cargoPkg{name: "x", dir: dir}
	jobs := c.jobs([]string{"cargo:x:lib:tests::a", "cargo:x:lib:tests::b", "cargo:x:tests/api"})
	if len(jobs) != 2 || jobs[0].target != "lib" || strings.Join(jobs[0].tests, ",") != "tests::a,tests::b" ||
		jobs[1].target != "tests/api" || jobs[1].tests != nil {
		t.Fatalf("%+v", jobs)
	}
	if all := c.jobs(nil); len(all) != 2 {
		t.Fatalf("%+v", all)
	}
}
