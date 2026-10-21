# Tests

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
session's exit status becomes the result.

rhun starts `go` and `cargo` for it as you, in your project, during the test runs you start: the
manifest asks for `tests = go cargo`, and the first run in a project asks you once.

Settings: `go_flags` (for example `-race`), `cargo_flags` (for example `--features full`).

Protocol: [rhun extension protocol 1.1, feature tests](https://rhun.app/extensions/v1/).
