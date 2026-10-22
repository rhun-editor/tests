# tests

Go and Rust tests in rhun's Testing view: run, debug, results with failure messages, an
expected/actual diff, and coverage tints.

- **Go**: every module (`go.mod`) in the project, its packages with `_test.go` files and their
  `TestXxx`, `BenchmarkXxx`, `FuzzXxx` and `ExampleXxx` (with an output comment) functions; subtests
  appear as they run. Runs are `go test -json`; coverage is `-coverprofile`.
- **Rust**: every Cargo package, its library, binary and `tests/` targets and their `#[test]`
  functions. Runs are `cargo test`; coverage needs
  [cargo-llvm-cov](https://github.com/taiki-e/cargo-llvm-cov).

Debug (`d`, or Debug Test at Cursor) runs the test under rhun's debugger: Delve (`dlv`, in test
mode on its package) for Go, lldb-dap on the test binary `cargo test --no-run` builds for Rust; the
session's exit status becomes the result, and Cancel stops the session.

Install it from rhun: the extensions button in the title bar, **Install from GitHub…**, then
`rhun-editor/tests`. It needs rhun 0.18 (protocol 1.1, feature `tests`), and Go or Cargo for
the projects you test.

## Settings

- **Extra go test flags** (`go_flags`): added to every `go test`, for example `-race` or
  `-count=1`.
- **Extra cargo test flags** (`cargo_flags`): added to every `cargo test`, for example
  `--features full`.

## Needs

Documents and project read (the test files and modules). Tests: rhun starts `go` and `cargo` for
it as you, in your project, during the test runs you start; the first run in a project asks you
once. Debug: the debuggers above, for a test run with the debug profile.

## Build and test

```sh
go test ./...           # discovery and the go test, cargo and coverage readers
tests/run.sh [RHUN]     # headless: discovery, a run with its failures and Go coverage
tests/check.sh          # the protocol's conformance checker
```

`tests/run.sh` runs the fixtures in `tests/fixtures` (a Go module, and a Cargo package when
`cargo` is on `PATH`). A tag `vX.Y.Z` equal to `version` in `extension.ini` builds Linux, macOS
and Windows and publishes what Install from GitHub takes. The protocol:
[feature tests](https://rhun.app/extensions/v1.1/).
