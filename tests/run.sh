#!/bin/sh
# Builds tests, lays it out as rhun installs it, and runs the Go and Rust fixtures headlessly in a
# project holding both: discovery, a run with its failures, and Go coverage. usage: tests/run.sh [RHUN]
set -e
cd "$(dirname "$0")/.."
RHUN=${1:-$(command -v rhun || echo "$HOME/.local/bin/rhun")}
case "$(uname -s)-$(uname -m)" in
    Linux-x86_64) target=linux-x86_64 ;;
    Darwin-arm64) target=macos-arm64 ;;
    *) echo "no target for $(uname -s)-$(uname -m)"; exit 77 ;;
esac
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
dir=$work/exts/rhun-editor/tests
mkdir -p "$dir/bin" "$work/proj/rs" "$work/config/rhun"
CGO_ENABLED=0 go build -o "$dir/bin/tests-$target" .
sh tests/layout.sh "$dir"  # what the release ships: the manifest, README, LICENSE and the icon
cp -R tests/fixtures/godemo/. "$work/proj/"
cp -R tests/fixtures/rsdemo/. "$work/proj/rs/"
printf '[files]\nrestore_session = false\nrestore_project = false\n[updates]\ncheck = false\n[git]\nenabled = false\n[ui]\nsidebar = false\nagents_panel = false\n[editor]\ncursor_blink = false\n' > "$work/config/rhun/config"
cat > "$work/script" <<SCRIPT
wait 100
cmd testing
wait-test-items 2
cmd test_coverage_all
wait 800
dialog yes
wait-tests
cmd test_run_all
wait-tests
print-tests all
print-test-result tests go:example.com/demo/calc.TestGreet
print-test-result tests cargo:demo:lib:tests::adds_wrong
print-coverage $work/proj/calc/calc.go
SCRIPT
# rustup and cargo keep their toolchains under the real home (read before HOME changes below)
rustup_home=${RUSTUP_HOME:-$HOME/.rustup}
cargo_home=${CARGO_HOME:-$HOME/.cargo}
HOME=$work XDG_CONFIG_HOME=$work/config XDG_STATE_HOME=$work/state \
    RUSTUP_HOME=$rustup_home CARGO_HOME=$cargo_home \
    "$RHUN" "$work/proj" --headless 1000x600 --scale 1 --extensions "$work/exts" --script "$work/script" \
    > "$work/out.txt" 2>&1
fail=0
check() {
    if grep -qF -- "$1" "$work/out.txt"; then echo "ok   $1"; else echo "FAIL $1"; fail=1; fi
}
check "failed case#01 id=go:example.com/demo/calc.TestAdd/case#01 kind=test"
check "skipped TestSkip id=go:example.com/demo/calc.TestSkip kind=test"
check "expected=Hello, rhun!\nWelcome. actual=Hello, rhun"
check "covered=5 partial=1 missed=2 lines=3:C,4:P,5:M,6:M,7:C,10:C,11:C,12:C"
if command -v cargo >/dev/null; then
    check "passed tests::adds id=cargo:demo:lib:tests::adds kind=test"
    check "skipped tests::slow id=cargo:demo:lib:tests::slow kind=test"
    check "at=$work/proj/rs/src/lib.rs:16:9 expected=5 actual=4"
fi
if [ $fail != 0 ]; then
    cat "$work/out.txt"; echo "--- the extension's log:"; cat "$work"/state/rhun/extensions/*.log 2>/dev/null | tail -20
    exit 1
fi
