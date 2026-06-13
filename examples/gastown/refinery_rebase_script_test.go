package gastown_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// refineryRebaseScript returns the absolute path to the shipped helper.
func refineryRebaseScript(t *testing.T) string {
	t.Helper()
	return filepath.Join(exampleDir(),
		"packs", "gastown", "assets", "scripts", "refinery-rebase.sh")
}

// initRebaseTestRepo sets up a bare "origin" repository plus a local clone,
// ready to host branches the refinery-rebase test cases will exercise. It
// returns the path to the local clone (the worktree the script runs in).
//
// Layout:
//
//	originDir  — bare repo at <tmp>/origin.git
//	cloneDir   — working clone at <tmp>/clone
//
// The clone has `main` initialized with a single seed commit and is
// configured with a committer identity so the test does not depend on the
// developer's global git config.
func initRebaseTestRepo(t *testing.T) (cloneDir string) {
	t.Helper()
	tmp := t.TempDir()
	originDir := filepath.Join(tmp, "origin.git")
	cloneDir = filepath.Join(tmp, "clone")

	mustGit := func(dir string, args ...string) {
		t.Helper()
		full := append([]string{"-C", dir}, args...)
		cmd := exec.Command("git", full...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.invalid",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	mustRun := func(dir, name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.invalid",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
	}

	if err := os.MkdirAll(originDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", originDir, err)
	}
	mustRun(originDir, "git", "-c", "init.defaultBranch=main", "init", "--bare", "-q")

	if err := os.MkdirAll(cloneDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", cloneDir, err)
	}
	mustRun(cloneDir, "git", "-c", "init.defaultBranch=main", "init", "-q")
	mustGit(cloneDir, "remote", "add", "origin", originDir)
	mustGit(cloneDir, "config", "user.email", "test@example.invalid")
	mustGit(cloneDir, "config", "user.name", "test")

	if err := os.WriteFile(filepath.Join(cloneDir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(cloneDir, "add", "seed.txt")
	mustGit(cloneDir, "commit", "-q", "-m", "seed")
	mustGit(cloneDir, "branch", "-M", "main")
	mustGit(cloneDir, "push", "-q", "origin", "main")
	return cloneDir
}

// gitInRepo runs git in the given working tree with a deterministic
// committer identity and no inherited global config.
func gitInRepo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitInRepoAllowErr(dir string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runRefineryRebase invokes the helper and returns its stderr and exit
// code. The script writes operational details to stderr (the success log
// tail and the conflict summary line), which is the only stream callers
// in these tests inspect.
func runRefineryRebase(t *testing.T, repo, branch, target string) (stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(refineryRebaseScript(t), branch, target)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("run script: %v", err)
		}
	}
	return errBuf.String(), exitCode
}

// TestRefineryRebaseScriptHasPipefail enforces the load-bearing line of
// defense the script exists to encode (acceptance criterion 1 on ga-vnr).
// Without `set -o pipefail`, future drift could reintroduce
// `git rebase ... | tail` patterns that mask non-zero exits.
func TestRefineryRebaseScriptHasPipefail(t *testing.T) {
	body, err := os.ReadFile(refineryRebaseScript(t))
	if err != nil {
		t.Fatalf("reading refinery-rebase.sh: %v", err)
	}
	src := string(body)
	if !strings.Contains(src, "set -euo pipefail") {
		t.Fatalf("refinery-rebase.sh missing `set -euo pipefail`:\n%s", src)
	}
}

// TestRefineryRebaseScriptExitsZeroOnCleanRebase covers the happy path:
// a feature branch that touches a different file than the target rebases
// cleanly and the working branch is left on `temp`.
func TestRefineryRebaseScriptExitsZeroOnCleanRebase(t *testing.T) {
	repo := initRebaseTestRepo(t)

	// Feature branch touches feature.txt only — no conflict with main.
	gitInRepo(t, repo, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, repo, "add", "feature.txt")
	gitInRepo(t, repo, "commit", "-q", "-m", "feature work")
	gitInRepo(t, repo, "push", "-q", "origin", "feature")

	// Main advances on a non-conflicting file.
	gitInRepo(t, repo, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(repo, "main.txt"), []byte("m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, repo, "add", "main.txt")
	gitInRepo(t, repo, "commit", "-q", "-m", "main forward")
	gitInRepo(t, repo, "push", "-q", "origin", "main")

	// Pre-rebase: feature.txt is not in main.
	if _, err := gitInRepoAllowErr(repo, "cat-file", "-e", "origin/main:feature.txt"); err == nil {
		t.Fatalf("precondition: feature.txt should not yet exist on origin/main")
	}

	stderr, code := runRefineryRebase(t, repo, "feature", "main")
	if code != 0 {
		t.Fatalf("clean rebase: exit=%d stderr=%q", code, stderr)
	}

	if got := gitInRepo(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "temp" {
		t.Fatalf("after success, HEAD on %q, want %q", got, "temp")
	}
	// Rebased history must contain both the new main commit and the feature commit.
	log := gitInRepo(t, repo, "log", "--format=%s", "-n", "3")
	for _, want := range []string{"feature work", "main forward", "seed"} {
		if !strings.Contains(log, want) {
			t.Fatalf("rebased log missing %q:\n%s", want, log)
		}
	}
}

// TestRefineryRebaseScriptExitsNonZeroOnConflict is the regression test
// the bead calls out (acceptance criterion 2 on ga-vnr): fabricate a
// rebase conflict and assert the script exits non-zero. The historic
// bug was that the earlier `git rebase | tail` wrapper masked the
// non-zero exit, so the caller proceeded as if the rebase had succeeded.
func TestRefineryRebaseScriptExitsNonZeroOnConflict(t *testing.T) {
	repo := initRebaseTestRepo(t)

	// Feature branch edits seed.txt.
	gitInRepo(t, repo, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, repo, "commit", "-q", "-am", "feature edits seed")
	gitInRepo(t, repo, "push", "-q", "origin", "feature")

	// Main edits the same file on the same line — guaranteed conflict on rebase.
	gitInRepo(t, repo, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("main2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, repo, "commit", "-q", "-am", "main edits seed")
	gitInRepo(t, repo, "push", "-q", "origin", "main")

	stderr, code := runRefineryRebase(t, repo, "feature", "main")
	if code == 0 {
		t.Fatalf("expected non-zero exit on rebase conflict; got 0\nstderr=%s", stderr)
	}
	if code != 3 {
		t.Fatalf("expected exit code 3 (conflict), got %d\nstderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "Rebase conflict") {
		t.Fatalf("expected stderr to identify the conflict; got %q", stderr)
	}
	if !strings.Contains(stderr, "seed.txt") {
		t.Fatalf("expected stderr to name the conflicting file (seed.txt); got %q", stderr)
	}
}

// TestRefineryRebaseScriptCleansUpAfterConflict verifies the
// post-conflict invariants the refinery relies on before reassigning the
// work bead back to the polecat pool:
//   - no in-progress rebase state (.git/rebase-merge / .git/rebase-apply)
//   - no leftover `temp` branch
//   - working tree on the target branch with no unmerged paths
func TestRefineryRebaseScriptCleansUpAfterConflict(t *testing.T) {
	repo := initRebaseTestRepo(t)

	gitInRepo(t, repo, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, repo, "commit", "-q", "-am", "feature edits seed")
	gitInRepo(t, repo, "push", "-q", "origin", "feature")

	gitInRepo(t, repo, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("main2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, repo, "commit", "-q", "-am", "main edits seed")
	gitInRepo(t, repo, "push", "-q", "origin", "main")

	_, code := runRefineryRebase(t, repo, "feature", "main")
	if code != 3 {
		t.Fatalf("expected conflict exit 3, got %d", code)
	}

	for _, stateDir := range []string{".git/rebase-merge", ".git/rebase-apply"} {
		if _, err := os.Stat(filepath.Join(repo, stateDir)); err == nil {
			t.Fatalf("expected %s to be cleaned up after rebase abort", stateDir)
		}
	}
	branches := gitInRepo(t, repo, "branch", "--list", "temp")
	if strings.TrimSpace(branches) != "" {
		t.Fatalf("expected `temp` branch to be deleted; got %q", branches)
	}
	if got := gitInRepo(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Fatalf("expected HEAD on main after conflict cleanup, got %q", got)
	}
	status := gitInRepo(t, repo, "status", "--porcelain")
	if status != "" {
		t.Fatalf("expected clean status after conflict cleanup; got %q", status)
	}
}

// TestRefineryRebaseScriptWritesReportFile asserts the optional
// REFINERY_REBASE_REPORT side-channel: when the caller sets that
// environment variable to a path, a conflict run writes the same
// one-line summary it printed on stderr. The refinery uses this to
// thread conflict file names into the rejection_reason metadata on the
// work bead so the next polecat sees what to fix.
func TestRefineryRebaseScriptWritesReportFile(t *testing.T) {
	repo := initRebaseTestRepo(t)

	gitInRepo(t, repo, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, repo, "commit", "-q", "-am", "feature edits seed")
	gitInRepo(t, repo, "push", "-q", "origin", "feature")

	gitInRepo(t, repo, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("main2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, repo, "commit", "-q", "-am", "main edits seed")
	gitInRepo(t, repo, "push", "-q", "origin", "main")

	reportPath := filepath.Join(t.TempDir(), "report.txt")
	cmd := exec.Command(refineryRebaseScript(t), "feature", "main")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"REFINERY_REBASE_REPORT="+reportPath,
	)
	if err := cmd.Run(); err == nil {
		t.Fatalf("expected non-zero exit on conflict")
	}

	body, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("reading report: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "Rebase conflict") || !strings.Contains(got, "seed.txt") {
		t.Fatalf("report did not capture conflict + file: %q", got)
	}
}

// TestRefineryRebaseScriptRejectsMissingArgs guards the script's input
// contract — passing fewer than two arguments exits 2 with a usage line.
func TestRefineryRebaseScriptRejectsMissingArgs(t *testing.T) {
	repo := initRebaseTestRepo(t)
	stderr, code := runRefineryRebase(t, repo, "", "")
	if code != 2 {
		t.Fatalf("expected usage exit 2, got %d (stderr=%q)", code, stderr)
	}
	if !strings.Contains(stderr, "usage:") {
		t.Fatalf("expected usage hint on stderr, got %q", stderr)
	}
}

// TestRefineryRebaseScriptRecoversFromLeftoverTempBranch ensures the
// script is re-entrant: a `temp` branch surviving from an aborted prior
// run does not block a fresh rebase.
func TestRefineryRebaseScriptRecoversFromLeftoverTempBranch(t *testing.T) {
	repo := initRebaseTestRepo(t)

	gitInRepo(t, repo, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, repo, "add", "feature.txt")
	gitInRepo(t, repo, "commit", "-q", "-m", "feature work")
	gitInRepo(t, repo, "push", "-q", "origin", "feature")

	gitInRepo(t, repo, "checkout", "-q", "main")
	gitInRepo(t, repo, "branch", "temp", "main")

	stderr, code := runRefineryRebase(t, repo, "feature", "main")
	if code != 0 {
		t.Fatalf("expected clean rebase even with leftover temp branch; exit=%d stderr=%q", code, stderr)
	}
}

// TestRefineryRebaseScriptAutostashesLocalModification covers gu-spq5/ga-wp1:
// the refinery worktree carries an uncommitted local modification to a tracked
// file (in production, `.beads/metadata.json`, whose dolt-server bookkeeping
// the refinery rewrites on every run). A plain `git rebase` refuses to start
// while the working tree is dirty ("cannot rebase: you have unstaged changes"),
// so the script mistook that for a conflict and exited 3 even though the branch
// rebases cleanly. `git rebase --autostash` stashes the local mod, rebases, and
// reapplies it — so a clean branch exits 0 and the local modification survives.
// Genuine branch/target conflicts still surface (see
// TestRefineryRebaseScriptExitsNonZeroOnConflict).
func TestRefineryRebaseScriptAutostashesLocalModification(t *testing.T) {
	repo := initRebaseTestRepo(t)

	// Feature branch touches feature.txt only — rebases cleanly onto main.
	gitInRepo(t, repo, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, repo, "add", "feature.txt")
	gitInRepo(t, repo, "commit", "-q", "-m", "feature work")
	gitInRepo(t, repo, "push", "-q", "origin", "feature")

	// Main advances on a non-conflicting file.
	gitInRepo(t, repo, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(repo, "main.txt"), []byte("m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInRepo(t, repo, "add", "main.txt")
	gitInRepo(t, repo, "commit", "-q", "-m", "main forward")
	gitInRepo(t, repo, "push", "-q", "origin", "main")

	// Dirty the worktree with an uncommitted modification to a tracked file
	// that is identical across main and origin/feature, so it carries into the
	// `temp` checkout — exactly the `.beads/metadata.json` situation. Without
	// --autostash this dirty state aborts the rebase.
	const localMod = "local refinery bookkeeping\n"
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte(localMod), 0o644); err != nil {
		t.Fatal(err)
	}

	stderr, code := runRefineryRebase(t, repo, "feature", "main")
	if code != 0 {
		t.Fatalf("autostash rebase: expected clean exit 0 with a dirty worktree, got exit=%d stderr=%q", code, stderr)
	}

	// Rebased history must contain both the new main commit and the feature commit.
	log := gitInRepo(t, repo, "log", "--format=%s", "-n", "3")
	for _, want := range []string{"feature work", "main forward", "seed"} {
		if !strings.Contains(log, want) {
			t.Fatalf("rebased log missing %q:\n%s", want, log)
		}
	}

	// --autostash must reapply the local modification, not discard it.
	got, err := os.ReadFile(filepath.Join(repo, "seed.txt"))
	if err != nil {
		t.Fatalf("reading seed.txt after autostash rebase: %v", err)
	}
	if string(got) != localMod {
		t.Fatalf("autostash did not restore the local modification; seed.txt=%q want %q", string(got), localMod)
	}
}

// TestRefineryFormulaTeachesSafeRebasePattern enforces that the rebase
// step explicitly warns against the masking-pipe anti-pattern that
// caused ga-vnr, and points the refinery agent at the canonical script.
// Future edits that revert to ad-hoc inline rebases or remove the warning
// will fail this test.
func TestRefineryFormulaTeachesSafeRebasePattern(t *testing.T) {
	path := filepath.Join(exampleDir(),
		"packs", "gastown", "formulas", "mol-refinery-patrol.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading refinery formula: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		"refinery-rebase.sh",
		"set -euo pipefail",
		// The anti-pattern warning is the durable bit — losing it would
		// silently re-enable the buggy `git rebase ... | tail` shape.
		"| tail",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("refinery formula missing safety guidance %q", want)
		}
	}
}

// TestRefineryPromptWarnsAgainstUnsafePipes mirrors the formula assertion
// at the prompt-template layer: the LLM agent reads the prompt at startup
// and needs the same anti-pattern warning so it does not regenerate the
// buggy wrapper from training data.
func TestRefineryPromptWarnsAgainstUnsafePipes(t *testing.T) {
	path := filepath.Join(exampleDir(),
		"packs", "gastown", "agents", "refinery", "prompt.template.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading refinery prompt: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		"refinery-rebase.sh",
		"| tail",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("refinery prompt missing safety guidance %q", want)
		}
	}
}

// stripInlineCode replaces markdown inline-code spans (`...`) with
// equally-sized whitespace so that index positions are preserved. The
// audit then operates on the prose-only view, so an anti-pattern cited
// inside `inline code` is exempt while bare bash on the same line is
// not.
func stripInlineCode(line string) string {
	out := []byte(line)
	inCode := false
	for i, c := range out {
		if c == '`' {
			inCode = !inCode
			out[i] = ' '
			continue
		}
		if inCode {
			out[i] = ' '
		}
	}
	return string(out)
}

// nearbyMentionsAntiPattern returns true if any of the up-to-five
// preceding non-blank lines flag the following code block as an
// intentional citation of the anti-pattern (e.g. "WRONG", "anti-pattern",
// "ga-vnr"). The audit uses this to exempt fenced code blocks that
// document the bug instead of reintroducing it.
func nearbyMentionsAntiPattern(lines []string, i int) bool {
	seen := 0
	for j := i - 1; j >= 0 && seen < 5; j-- {
		stripped := strings.TrimSpace(lines[j])
		if stripped == "" || strings.HasPrefix(stripped, "```") {
			continue
		}
		seen++
		lower := strings.ToLower(stripped)
		for _, marker := range []string{"wrong", "anti-pattern", "ga-vnr", "do not", "never wrap", "never pipe"} {
			if strings.Contains(lower, marker) {
				return true
			}
		}
	}
	return false
}

// TestGastownPackRefineryHasNoUnsafePipesOnFailableCommands grep-audits
// the gastown pack for the historical anti-pattern: piping a command
// that can fail (rebase, push, fetch) through `tail` or `grep`. Without
// `set -o pipefail`, those pipes mask non-zero exits — the exact bug
// ga-vnr exists to prevent. Documented citations of the anti-pattern
// (inside `inline code` or fenced blocks preceded by "WRONG" /
// "anti-pattern" markers) are exempt; bare bash that reintroduces the
// pattern is not.
func TestGastownPackRefineryHasNoUnsafePipesOnFailableCommands(t *testing.T) {
	roots := []string{
		filepath.Join(exampleDir(), "packs", "gastown", "formulas"),
		filepath.Join(exampleDir(), "packs", "gastown", "agents", "refinery"),
	}
	unsafe := []string{
		"git rebase ",
		"git push ",
		"git fetch ",
		"gh pr create ",
	}
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			lines := strings.Split(string(data), "\n")
			inFence := false
			fenceIsCitation := false
			for i, line := range lines {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "```") {
					if inFence {
						inFence = false
						fenceIsCitation = false
					} else {
						inFence = true
						fenceIsCitation = nearbyMentionsAntiPattern(lines, i)
					}
					continue
				}
				if inFence && fenceIsCitation {
					continue
				}
				view := line
				if !inFence {
					// Outside fences, treat inline-code spans as citations.
					view = stripInlineCode(line)
				}
				if strings.HasPrefix(strings.TrimSpace(view), "#") ||
					strings.HasPrefix(strings.TrimSpace(view), ">") {
					continue
				}
				for _, cmd := range unsafe {
					idx := strings.Index(view, cmd)
					if idx < 0 {
						continue
					}
					tail := view[idx:]
					if (strings.Contains(tail, "| tail") || strings.Contains(tail, "| grep")) &&
						!strings.Contains(tail, "tee ") {
						t.Errorf("%s:%d: unsafe pipe (`%s... | tail/grep`) on a failable command:\n  %s",
							path, i+1, strings.TrimSpace(cmd), line)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
}

// extractRebaseScriptResolution returns the shell the rebase step uses to
// LOCATE refinery-rebase.sh, sliced out of the formula prose. It spans the
// opening of the fenced bash block down to (but not including) the
// `REPORT="$(mktemp ...)"` line — i.e. the path-resolution logic plus its
// not-found guard, without the actual script invocation. Keying the slice on
// the stable REPORT marker (unchanged by ga-7o3) keeps the helper valid
// across future edits to the resolution itself.
func extractRebaseScriptResolution(t *testing.T) string {
	t.Helper()
	path := filepath.Join(exampleDir(),
		"packs", "gastown", "formulas", "mol-refinery-patrol.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading refinery formula: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	reportIdx := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), `REPORT="$(mktemp`) {
			reportIdx = i
			break
		}
	}
	if reportIdx < 0 {
		t.Fatal("could not find REPORT marker in formula; rebase resolution block moved")
	}
	fenceIdx := -1
	for i := reportIdx - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "```bash" {
			fenceIdx = i
			break
		}
	}
	if fenceIdx < 0 {
		t.Fatal("could not find opening bash fence before REPORT marker")
	}
	return strings.Join(lines[fenceIdx+1:reportIdx], "\n")
}

// TestRefineryFormulaResolvesRebaseScriptAcrossInstallLayouts executes the
// rebase step's script-resolution shell against the install layouts a
// refinery encounters and asserts it locates refinery-rebase.sh in each.
//
// The load-bearing case is "system packs only" (ga-7o3): a refinery worktree
// that carries no nested pack while runtime/packs has not synced gastown. The
// original resolution probed only $GC_CITY_RUNTIME_DIR/packs/gastown plus a
// working-tree `find`, so it STOPped the first merge with "refinery-rebase.sh
// not found". The fix adds a <city>/.gc/system/packs probe — the canonical
// installed-pack location, always present even when runtime/packs omits
// gastown and the worktree carries no nested pack.
func TestRefineryFormulaResolvesRebaseScriptAcrossInstallLayouts(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	resolution := extractRebaseScriptResolution(t)
	// Relative to a `packs` root, the helper always lives here.
	relScript := filepath.Join("gastown", "assets", "scripts", "refinery-rebase.sh")

	// Stub `gc` so the not-found guard's `gc runtime drain-ack` cannot reach
	// the real CLI on the failure path (pre-fix, and defensively forever).
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "gc"), "#!/bin/sh\nexit 0\n")

	writeFakeScript := func(t *testing.T, packsRoot string) string {
		t.Helper()
		full := filepath.Join(packsRoot, relScript)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, full, "#!/bin/sh\nexit 0\n")
		return full
	}

	cases := []struct {
		name string
		// setup builds the layout under city and returns the working
		// directory plus the script path the resolution must select. An
		// empty wantScript means "any path ending in relScript" (the
		// dev-checkout `find` fallback prints a ./-relative path).
		setup func(t *testing.T, city string) (workdir, wantScript string)
	}{
		{
			name: "system packs only (worktree without nested pack)",
			setup: func(t *testing.T, city string) (string, string) {
				// runtime/packs exists but deliberately lacks gastown.
				if err := os.MkdirAll(filepath.Join(city, ".gc", "runtime", "packs"), 0o755); err != nil {
					t.Fatal(err)
				}
				want := writeFakeScript(t, filepath.Join(city, ".gc", "system", "packs"))
				work := filepath.Join(city, "work")
				if err := os.MkdirAll(work, 0o755); err != nil {
					t.Fatal(err)
				}
				return work, want
			},
		},
		{
			name: "runtime packs takes priority when present",
			setup: func(t *testing.T, city string) (string, string) {
				want := writeFakeScript(t, filepath.Join(city, ".gc", "runtime", "packs"))
				writeFakeScript(t, filepath.Join(city, ".gc", "system", "packs"))
				work := filepath.Join(city, "work")
				if err := os.MkdirAll(work, 0o755); err != nil {
					t.Fatal(err)
				}
				return work, want
			},
		},
		{
			name: "dev-checkout fallback walks the working tree",
			setup: func(t *testing.T, city string) (string, string) {
				// Neither runtime/packs nor system/packs carry gastown; the
				// only copy lives in a nested pack under the working tree.
				work := filepath.Join(city, "checkout")
				writeFakeScript(t, filepath.Join(work, "packs"))
				return work, ""
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			work, want := tc.setup(t, city)

			cmd := exec.Command("bash", "-c", resolution+"\nprintf '%s' \"$SCRIPT\"\n")
			cmd.Dir = work
			cmd.Env = mergeTestEnv(map[string]string{
				"GC_CITY":             city,
				"GC_CITY_PATH":        city,
				"GC_CITY_RUNTIME_DIR": filepath.Join(city, ".gc", "runtime"),
				"BRANCH":              "test-branch",
				"PATH":                binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			})

			out, err := cmd.Output()
			if err != nil {
				stderr := ""
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					stderr = string(ee.Stderr)
				}
				t.Fatalf("resolution snippet failed to locate refinery-rebase.sh: %v\nstderr:\n%s\nsnippet:\n%s",
					err, stderr, resolution)
			}
			got := strings.TrimSpace(string(out))
			if want != "" {
				if got != want {
					t.Fatalf("resolved SCRIPT = %q, want %q", got, want)
				}
			} else if !strings.HasSuffix(got, relScript) {
				t.Fatalf("resolved SCRIPT = %q, want suffix %q", got, relScript)
			}
		})
	}
}
