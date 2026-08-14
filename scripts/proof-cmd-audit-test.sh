#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

cat >"$tmp/bad.json" <<'EOF'
{"tasks":[
 {"key":"Z-MISSING","public_id":"z","status":"done","proof_cmd":"go test -run TestCertainlyAbsent ./internal/ids"},
 {"key":"Y-EQUALS","public_id":"y","status":"done","proof_cmd":"go test -run=TestAlsoCertainlyAbsent ./internal/ids"},
 {"key":"X-QUOTED","public_id":"x","status":"done","proof_cmd":"go test -run \"TestQuotedAbsent\" ./internal/ids"},
 {"key":"A-MASK","public_id":"a","status":"done","proof_cmd":"go test ./internal/ids | true"},
 {"key":"B-BRANCH","public_id":"b","status":"done","proof_cmd":"if test -s nope; then echo ok; exit 0; else printf no; true; fi; if test x; then :; else exit 0; fi"},
 {"key":"W-REGEX","public_id":"n1","status":"done","proof_cmd":"go test -run 'Test[N]everPresent' ./internal/ids"},
 {"key":"N-DYNAMIC","public_id":"n2","status":"done","proof_cmd":"go test -run TestAbsent ./internal/..."}
]}
EOF
set +e
python3 scripts/proof-cmd-audit.py --input "$tmp/bad.json" >"$tmp/out"
rc=$?
set -e
test "$rc" -eq 1
test "$(wc -l <"$tmp/out")" -eq 7
sed -n '1p' "$tmp/out" | grep -q $'^high\tB-BRANCH\tboth-success-branches\t'
test "$(grep -c $'^high\tB-BRANCH\tboth-success-branches\t' "$tmp/out")" -eq 2
grep -q $'^high\tA-MASK\tsuccess-masking-pipeline\t' "$tmp/out"
grep -q $'^medium\tZ-MISSING\tmissing-go-test-target\t' "$tmp/out"
# All three exact spellings are recognized; missing findings sort by task key.
grep -q $'^medium\tX-QUOTED\tmissing-go-test-target\t' "$tmp/out"
grep -q $'^medium\tY-EQUALS\tmissing-go-test-target\t' "$tmp/out"
# Character classes are outside the authoritative literal-only scope.
! grep -q $'\tW-REGEX\t' "$tmp/out"

printf '%s\n' '{"tasks":[{"key":"OK","public_id":"ok","status":"done","proof_cmd":"go test -run ^TestAgentIDMintingRoundTrip$ ./internal/ids"},{"key":"PREFIX","public_id":"prefix","status":"done","proof_cmd":"go test -run TestAgentIDMinting ./internal/ids"},{"key":"HOSTILE","public_id":"hostile","status":"done","proof_cmd":"go test -run Test(a+)+$ ./internal/ids"}]}' >"$tmp/clean.json"
# A catastrophic regex shape is skipped promptly and never evaluated.
timeout 2 python3 scripts/proof-cmd-audit.py --input "$tmp/clean.json" | grep -qx 'proof-cmd-audit: no findings'
python3 scripts/proof-cmd-audit.py --input "$tmp/clean.json" | grep -qx 'proof-cmd-audit: no findings'

# Thousands of proofs sharing one package reuse its bounded canonical scan.
python3 - "$tmp/many.json" <<'PY'
import json, sys
tasks = [{"key":f"MANY-{i}", "public_id":str(i), "status":"done",
          "proof_cmd":"go test -run TestAgentIDMinting ./internal/ids"} for i in range(3000)]
json.dump({"tasks":tasks}, open(sys.argv[1], "w", encoding="utf-8"))
PY
timeout 10 python3 scripts/proof-cmd-audit.py --input "$tmp/many.json" | grep -qx 'proof-cmd-audit: no findings'

python3 scripts/proof-cmd-audit.py --json --input "$tmp/bad.json" >"$tmp/one.json" || test "$?" -eq 1
python3 scripts/proof-cmd-audit.py --json --input "$tmp/bad.json" >"$tmp/two.json" || test "$?" -eq 1
cmp "$tmp/one.json" "$tmp/two.json"
python3 - "$tmp/one.json" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1], encoding="utf-8"))
assert doc["finding_count"] == 6
PY

printf '%s\n' '{broken' >"$tmp/malformed.json"
set +e
python3 scripts/proof-cmd-audit.py --input "$tmp/malformed.json" >/dev/null 2>&1
rc=$?
set -e
test "$rc" -eq 2
printf '%s\n' '[]' >"$tmp/not-object.json"
set +e
python3 scripts/proof-cmd-audit.py --input "$tmp/not-object.json" >/dev/null 2>&1
rc=$?
set -e
test "$rc" -eq 2
printf '%s\n' '{"tasks":[{"key":7,"public_id":"p","status":"done","proof_cmd":null}]}' >"$tmp/bad-schema.json"
set +e
python3 scripts/proof-cmd-audit.py --input "$tmp/bad-schema.json" >/dev/null 2>&1
rc=$?
set -e
test "$rc" -eq 2

# Control characters cannot create extra text records or terminal escapes.
python3 - "$tmp/control.json" <<'PY'
import json, sys
with open(sys.argv[1], "w", encoding="utf-8") as out:
    json.dump({"tasks":[{"key":"BAD\u009b[2J\u202eKEY\nNEXT", "public_id":"p",
                         "status":"done", "proof_cmd":"go test ./internal/ids | true"}]}, out)
PY
set +e
python3 scripts/proof-cmd-audit.py --input "$tmp/control.json" >"$tmp/control.out"
rc=$?
set -e
test "$rc" -eq 1
test "$(wc -l <"$tmp/control.out")" -eq 2
test "$(LC_ALL=C tr -cd '\033' <"$tmp/control.out" | wc -c)" -eq 0
! grep -q $'\302\233\|\342\200\256' "$tmp/control.out"

# A symlink masquerading as Go test source is skipped, never followed.
python3 - "$tmp" <<'PY'
import importlib.util, pathlib, sys
sys.dont_write_bytecode = True
root = pathlib.Path(sys.argv[1]); pkg = root / "pkg"; pkg.mkdir()
target = root / "outside"; target.write_text("func TestSecret(\n", encoding="utf-8")
(pkg / "secret_test.go").symlink_to(target)
spec = importlib.util.spec_from_file_location("audit", "scripts/proof-cmd-audit.py")
audit = importlib.util.module_from_spec(spec); spec.loader.exec_module(audit); audit.ROOT = root
task = {"key":"LINK", "public_id":"l", "status":"done", "proof_cmd":"go test -run TestSecret ./pkg"}
assert audit.audit_command(task) == []
PY

# Comment and literal contents cannot impersonate top-level Go tests.
python3 - "$tmp" <<'PY'
import importlib.util, pathlib, sys
sys.dont_write_bytecode = True
root = pathlib.Path(sys.argv[1]) / "lex"; pkg = root / "pkg"; pkg.mkdir(parents=True)
(pkg / "spoof_test.go").write_text('''package pkg
// func TestComment(
/* func TestBlock( */
var a = "func TestString("
var b = `func TestRaw(`
var c = 'x' // func TestRuneTail(
func TestActual(t *testing.T) {}
''', encoding="utf-8")
spec = importlib.util.spec_from_file_location("audit", "scripts/proof-cmd-audit.py")
audit = importlib.util.module_from_spec(spec); spec.loader.exec_module(audit); audit.ROOT = root
def kinds(name):
    task={"key":"LEX","public_id":"l","status":"done","proof_cmd":f"go test -run {name} ./pkg"}
    return [x["kind"] for x in audit.audit_command(task)]
assert kinds("TestComment") == ["missing-go-test-target"]
assert kinds("TestBlock") == ["missing-go-test-target"]
assert kinds("TestString") == ["missing-go-test-target"]
assert kinds("TestRaw") == ["missing-go-test-target"]
assert kinds("TestRuneTail") == ["missing-go-test-target"]
assert kinds("TestAct") == []
PY

# Aggregate source budget overflow is a clean input error (exit 2).
python3 - "$tmp" <<'PY'
import contextlib, importlib.util, io, pathlib, sys
sys.dont_write_bytecode = True
root = pathlib.Path(sys.argv[1]) / "cap"; pkg = root / "pkg"; pkg.mkdir(parents=True)
(pkg / "large_test.go").write_text("package pkg\nfunc TestLarge(t *testing.T) {}\n", encoding="utf-8")
spec = importlib.util.spec_from_file_location("audit", "scripts/proof-cmd-audit.py")
audit = importlib.util.module_from_spec(spec); spec.loader.exec_module(audit)
audit.ROOT = root; audit.MAX_SOURCE_BYTES = 1
audit.load_export = lambda unused: {"tasks":[{"key":"CAP", "public_id":"c", "status":"done",
                                              "proof_cmd":"go test -run TestLarge ./pkg"}]}
old = sys.argv; sys.argv = ["proof-cmd-audit.py"]
try:
    with contextlib.redirect_stderr(io.StringIO()):
        assert audit.main() == 2
finally:
    sys.argv = old
PY

# Default mode is one read-only export request; the fake rejects any mutation.
cat >"$tmp/spec-cloud" <<EOF
#!/usr/bin/env bash
test "\$*" = '-sf /api/v1/projects/agent-bus/export?format=json' || exit 91
cat '$tmp/clean.json'
EOF
chmod +x "$tmp/spec-cloud"
PROOF_CMD_AUDIT_SPEC_CLOUD="$tmp/spec-cloud" python3 scripts/proof-cmd-audit.py >/dev/null

echo 'proof-cmd-audit-test: PASS'
