// Tests for the gascity local vet gate (ga-9m26, di-sgjx, di-0pmi).
//
// .githooks/pre-push runs `go vet ./...` before every push, so DIRECT
// commits/pushes (a mayor or human hotfix outside the polecat -> refinery
// pipeline) face the same gate the pipeline already enforces. Because the
// gascity repo has no city.toml to read the gate from, the command is
// hard-coded in the hook; these tests pin it to the Makefile `vet` target
// (drift guard) and exercise the gate + fail-open + escape-hatch behavior.
//
// The hook is activated by `make setup` (git config core.hooksPath=.githooks),
// the same mechanism that activates .githooks/pre-commit — no copy-installer
// is needed, and one would be inert under core.hooksPath anyway.
package packlint

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// prePushGate is the gate the hook must run. It mirrors the gascity rig's
// `lint_command` (dipcity city.toml) and the Makefile `vet` target.
const prePushGate = "go vet ./..."

func prePushHookPath() string {
	return filepath.Join(repoRoot(), ".githooks", "pre-push")
}

// cleanHookEnv returns the host env with anything that would perturb the
// hook's behavior removed (ambient GASCITY_* gate knobs and GC_* session
// vars). PATH/GOROOT/GOCACHE survive so a child `go vet` still resolves.
func cleanHookEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GASCITY_") || strings.HasPrefix(kv, "GC_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

// runPrePush invokes the hook as git would (`pre-push <remote> <url>`) from
// cwd, with the given extra env entries appended to a clean host env.
func runPrePush(t *testing.T, cwd string, extraEnv ...string) (stdout, stderr string, exit int) {
	t.Helper()
	cmd := exec.Command("sh", prePushHookPath(), "origin", "https://example")
	cmd.Dir = cwd
	cmd.Env = append(cleanHookEnv(), extraEnv...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	exit = 0
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("running pre-push hook: %v", err)
		}
		exit = exitErr.ExitCode()
	}
	return outBuf.String(), errBuf.String(), exit
}

// newGoModule creates a git-initialized temp dir holding a minimal Go module.
// When buggy is true, main.go carries a `go vet`-detectable Printf mismatch.
func newGoModule(t *testing.T, buggy bool) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/prepushgate\n\ngo 1.21\n")
	main := "package main\n\nfunc main() {}\n"
	if buggy {
		// `go vet`'s printf analyzer flags the %d/string mismatch; the package
		// still type-checks (Printf is variadic), so vet runs and reports.
		main = "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Printf(\"%d\\n\", \"not an int\")\n}\n"
	}
	writeFile(t, filepath.Join(dir, "main.go"), main)
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// TestPrePushHookIsExecutable: the shipped hook must exist and be executable,
// or core.hooksPath would silently skip it.
func TestPrePushHookIsExecutable(t *testing.T) {
	info, err := os.Stat(prePushHookPath())
	if err != nil {
		t.Fatalf("stat pre-push hook: %v", err)
	}
	if info.Mode()&0o100 == 0 {
		t.Errorf("%s must be executable (mode %v)", prePushHookPath(), info.Mode())
	}
}

// TestPrePushHookGateMatchesMakefileVet is the drift guard: the hook's gate,
// the Makefile `vet` recipe, and the expected constant must all agree. Because
// gascity has no city.toml, the hook hard-codes the gate; this test is what
// stops it from drifting away from the pipeline's `go vet ./...`.
func TestPrePushHookGateMatchesMakefileVet(t *testing.T) {
	// The hook's own view of the gate, via its dry-run seam.
	stdout, stderr, exit := runPrePush(t, repoRoot(), "GASCITY_LINT_HOOK_DRYRUN=1")
	if exit != 0 {
		t.Fatalf("dry-run exited %d: %s", exit, stderr)
	}
	if stdout != prePushGate {
		t.Errorf("hook gate = %q, want %q", stdout, prePushGate)
	}

	// The Makefile `vet` recipe must be the same command, so a change to one
	// flags the need to update the other.
	if got := makefileVetRecipe(t); got != prePushGate {
		t.Errorf("Makefile `vet` recipe = %q, want %q (keep it in sync with the pre-push hook)", got, prePushGate)
	}
}

// makefileVetRecipe returns the single-line recipe of the Makefile `vet`
// target (leading tab stripped).
func makefileVetRecipe(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), "Makefile"))
	if err != nil {
		t.Fatalf("reading Makefile: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if line != "vet:" {
			continue
		}
		var recipe []string
		for _, body := range lines[i+1:] {
			if !strings.HasPrefix(body, "\t") {
				break
			}
			recipe = append(recipe, strings.TrimPrefix(body, "\t"))
		}
		return strings.Join(recipe, "\n")
	}
	t.Fatalf("no `vet:` target found in Makefile")
	return ""
}

// TestPrePushHookPassesCleanModule: a clean tree clears the gate (exit 0).
func TestPrePushHookPassesCleanModule(t *testing.T) {
	_, stderr, exit := runPrePush(t, newGoModule(t, false))
	if exit != 0 {
		t.Fatalf("clean module blocked (exit %d): %s", exit, stderr)
	}
	if !strings.Contains(stderr, "passed") {
		t.Errorf("expected a pass message, got: %s", stderr)
	}
}

// TestPrePushHookBlocksVetFailure: a `go vet` finding blocks the push (exit !=0).
func TestPrePushHookBlocksVetFailure(t *testing.T) {
	_, stderr, exit := runPrePush(t, newGoModule(t, true))
	if exit == 0 {
		t.Fatalf("vet failure did NOT block the push; stderr: %s", stderr)
	}
	if !strings.Contains(stderr, "FAILED") {
		t.Errorf("expected a FAILED message, got: %s", stderr)
	}
}

// TestPrePushHookSkipEnvBypassesFailure: GASCITY_SKIP_LINT_HOOK=1 lets even a
// vet-failing tree through — the documented escape hatch.
func TestPrePushHookSkipEnvBypassesFailure(t *testing.T) {
	_, stderr, exit := runPrePush(t, newGoModule(t, true), "GASCITY_SKIP_LINT_HOOK=1")
	if exit != 0 {
		t.Fatalf("skip env did not bypass the gate (exit %d): %s", exit, stderr)
	}
	if !strings.Contains(stderr, "skipping") {
		t.Errorf("expected a skip message, got: %s", stderr)
	}
}

// TestPrePushHookSkipsOutsideGitTree: outside any work tree the hook fails open
// (a non-repo dir has nothing to gate).
func TestPrePushHookSkipsOutsideGitTree(t *testing.T) {
	dir := t.TempDir() // not git-initialized
	// Stop git from discovering a repo above the temp dir on hosts whose
	// TMPDIR happens to live inside one.
	_, stderr, exit := runPrePush(t, dir, "GIT_CEILING_DIRECTORIES="+dir)
	if exit != 0 {
		t.Fatalf("outside a git tree the hook should skip (exit %d): %s", exit, stderr)
	}
	if !strings.Contains(stderr, "not inside a git work tree") {
		t.Errorf("expected a not-a-work-tree message, got: %s", stderr)
	}
}

// TestPrePushHookFailsOpenWhenGoMissing: with `go` absent from PATH the hook
// warns and allows the push (the refinery re-gate is authoritative). It must
// NOT fall through to running the gate.
func TestPrePushHookFailsOpenWhenGoMissing(t *testing.T) {
	dir := newGoModule(t, true) // would fail the gate if go were present
	path := minimalPathWithout(t, "go", "git", "sh")
	_, stderr, exit := runPrePush(t, dir, "PATH="+path)
	if exit != 0 {
		t.Fatalf("missing go toolchain should fail open (exit %d): %s", exit, stderr)
	}
	if !strings.Contains(stderr, "go not found") {
		t.Errorf("expected a go-not-found message, got: %s", stderr)
	}
}

// minimalPathWithout builds a one-dir PATH containing symlinks to the named
// tools, asserting `excluded` is not reachable through it.
func minimalPathWithout(t *testing.T, excluded string, tools ...string) string {
	t.Helper()
	binDir := t.TempDir()
	for _, tool := range tools {
		realPath, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("host must provide %q: %v", tool, err)
		}
		if err := os.Symlink(realPath, filepath.Join(binDir, tool)); err != nil {
			t.Fatalf("symlinking %q: %v", tool, err)
		}
	}
	if p, err := exec.LookPath(filepath.Join(binDir, excluded)); err == nil {
		t.Fatalf("excluded tool %q unexpectedly present at %s", excluded, p)
	}
	return binDir
}
