package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestBdSubcommandNeedsFormulas pins the gating: only molecule subcommands
// (`bd mol ...`) resolve the .beads/formulas/ symlink view, so only they
// trigger the self-heal. The hot CRUD/query path must NOT, to keep the
// work-query loop free of extra filesystem work.
func TestBdSubcommandNeedsFormulas(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"mol wisp", []string{"mol", "wisp", "mol-witness-patrol"}, true},
		{"mol bond", []string{"mol", "bond", "mol-x", "ga-1"}, true},
		{"mol pour", []string{"mol", "pour", "mol-x"}, true},
		{"bare mol", []string{"mol"}, true},
		{"list", []string{"list"}, false},
		{"ready", []string{"ready", "--json"}, false},
		{"show", []string{"show", "ga-1"}, false},
		{"update claim", []string{"update", "ga-1", "--claim"}, false},
		{"create", []string{"create", "--title", "x"}, false},
		{"empty", []string{}, false},
		// Subcommand-first convention (matches IsBdReadOnlySubcommand): a
		// global flag ahead of the subcommand is not classified.
		{"leading flag", []string{"--json", "mol", "wisp"}, false},
		// "molecule"/"molt" must not match a "mol" prefix — exact subcommand only.
		{"molt not mol", []string{"molt", "x"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bdSubcommandNeedsFormulas(c.args); got != c.want {
				t.Errorf("bdSubcommandNeedsFormulas(%v) = %v, want %v", c.args, got, c.want)
			}
		})
	}
}

// TestHealFormulaSymlinksForScope_RecreatesMissingSymlink reproduces ga-y9v:
// a pack rebuild momentarily removed a formula source file, ResolveFormulas's
// broken-link cleanup deleted the now-dangling .beads/formulas/ symlink, and
// nothing recreated it until a manual `gc reload`. Because the symlink target
// path is stable across materializations, the heal only has to re-resolve once
// the source file is back on disk for the witness/refinery `gc bd mol wisp`
// path to recover on its own.
func TestHealFormulaSymlinksForScope_RecreatesMissingSymlink(t *testing.T) {
	dir := t.TempDir()
	layer := filepath.Join(dir, "gastown", "formulas")
	writeFormulaFile(t, layer, "mol-witness-patrol.toml", "patrol")

	scope := filepath.Join(dir, "city")
	symlinkDir := filepath.Join(scope, ".beads", "formulas")
	if err := os.MkdirAll(symlinkDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{FormulaLayers: config.FormulaLayers{City: []string{layer}}}
	target := execStoreTarget{ScopeRoot: scope, ScopeKind: "city"}

	// Establish the healthy state, then simulate the bug: delete the symlinks
	// while the source file remains present (its post-rebuild state).
	healFormulaSymlinksForScope(cfg, target, &bytes.Buffer{})
	for _, n := range []string{"mol-witness-patrol.toml", "mol-witness-patrol.formula.toml"} {
		if _, err := os.Lstat(filepath.Join(symlinkDir, n)); err != nil {
			t.Fatalf("precondition: %s should exist after first heal: %v", n, err)
		}
		if err := os.Remove(filepath.Join(symlinkDir, n)); err != nil {
			t.Fatalf("pre-remove %s: %v", n, err)
		}
	}

	// Heal again — the missing symlinks must be recreated from the still-present
	// source file, no `gc reload` required.
	healFormulaSymlinksForScope(cfg, target, &bytes.Buffer{})

	want := filepath.Join(layer, "mol-witness-patrol.toml")
	for _, n := range []string{"mol-witness-patrol.toml", "mol-witness-patrol.formula.toml"} {
		dest, err := os.Readlink(filepath.Join(symlinkDir, n))
		if err != nil {
			t.Fatalf("%s not recreated by heal: %v", n, err)
		}
		if dest != want {
			t.Errorf("%s -> %q, want %q", n, dest, want)
		}
	}
}

// TestHealFormulaSymlinksForScope_RigScopeUsesRigLayers verifies the heal
// targets the rig's own .beads/formulas/ using rig-specific layers when bd
// resolved to a rig scope, so per-rig formulas (e.g. gastown imported via
// [rigs.imports.X]) are healed in the directory the rig-scoped bd reads.
func TestHealFormulaSymlinksForScope_RigScopeUsesRigLayers(t *testing.T) {
	dir := t.TempDir()
	cityLayer := filepath.Join(dir, "city", "formulas")
	rigLayer := filepath.Join(dir, "rigpack", "formulas")
	writeFormulaFile(t, cityLayer, "mol-city.toml", "city")
	writeFormulaFile(t, rigLayer, "mol-rig.toml", "rig")

	scope := filepath.Join(dir, "rigroot")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{FormulaLayers: config.FormulaLayers{
		City: []string{cityLayer},
		Rigs: map[string][]string{"myrig": {cityLayer, rigLayer}},
	}}
	target := execStoreTarget{ScopeRoot: scope, ScopeKind: "rig", RigName: "myrig"}

	healFormulaSymlinksForScope(cfg, target, &bytes.Buffer{})

	symlinkDir := filepath.Join(scope, ".beads", "formulas")
	for name, srcLayer := range map[string]string{
		"mol-city.toml": cityLayer,
		"mol-rig.toml":  rigLayer,
	} {
		dest, err := os.Readlink(filepath.Join(symlinkDir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := filepath.Join(srcLayer, name); dest != want {
			t.Errorf("%s -> %q, want %q", name, dest, want)
		}
	}
}

// TestHealFormulaSymlinksForScope_NoLayersNoop verifies the heal is a safe
// no-op when the scope has no formula layers (minimal city, no packs) and
// when cfg is nil — it must never create .beads/formulas/ or panic.
func TestHealFormulaSymlinksForScope_NoLayersNoop(t *testing.T) {
	dir := t.TempDir()
	scope := filepath.Join(dir, "city")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}

	healFormulaSymlinksForScope(&config.City{}, execStoreTarget{ScopeRoot: scope, ScopeKind: "city"}, &bytes.Buffer{})
	if _, err := os.Stat(filepath.Join(scope, ".beads", "formulas")); !os.IsNotExist(err) {
		t.Errorf(".beads/formulas must not be created when no layers configured (err=%v)", err)
	}

	// nil cfg must also be safe.
	healFormulaSymlinksForScope(nil, execStoreTarget{ScopeRoot: scope}, &bytes.Buffer{})
}

// TestDoBd_MolSubcommandHealsFormulaSymlinks is the end-to-end wiring guard:
// a `gc bd mol ...` invocation must re-materialize the .beads/formulas/
// symlink view (so the witness/refinery recover from a partial view without a
// manual reload), while a non-mol invocation must leave the view untouched.
func TestDoBd_MolSubcommandHealsFormulaSymlinks(t *testing.T) {
	disableManagedDoltRecoveryForTest(t)

	origCityFlag := cityFlag
	origRigFlag := rigFlag
	t.Cleanup(func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	})
	cityFlag = ""
	rigFlag = ""

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A city-local formula — ComputeFormulaLayers always folds <city>/formulas
	// into FormulaLayers.City, so the heal will resolve a symlink for it.
	writeFormulaFile(t, filepath.Join(cityDir, "formulas"), "mol-test.toml", "test formula")

	binDir := t.TempDir()
	script := filepath.Join(binDir, "bd")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
set -eu
while [ $# -gt 0 ]; do
  case "$1" in
    -*) shift ;;
    *) break ;;
  esac
done
case "${1:-}" in
  mol) printf '{}\n' ;;
  show) printf '[{"id":"gc-1","title":"ok"}]\n' ;;
  *) exit 2 ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("BD_EXPORT_AUTO", "true")

	symlinkDir := filepath.Join(cityDir, ".beads", "formulas")
	molLinks := []string{"mol-test.toml", "mol-test.formula.toml"}

	// 1. A mol command heals: the symlink view is materialized.
	var stdout, stderr bytes.Buffer
	if got := doBd([]string{"mol", "wisp", "mol-test", "--json"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd(mol wisp) = %d, want 0; stderr=%q", got, stderr.String())
	}
	for _, n := range molLinks {
		if _, err := os.Lstat(filepath.Join(symlinkDir, n)); err != nil {
			t.Fatalf("mol subcommand should have healed %s: %v", n, err)
		}
	}

	// 2. Simulate the post-rebuild partial state: delete the symlinks.
	for _, n := range molLinks {
		if err := os.Remove(filepath.Join(symlinkDir, n)); err != nil {
			t.Fatalf("remove %s: %v", n, err)
		}
	}

	// 3. A non-mol command must NOT heal (hot path stays untouched).
	stdout.Reset()
	stderr.Reset()
	if got := doBd([]string{"show", "gc-1", "--json"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd(show) = %d, want 0; stderr=%q", got, stderr.String())
	}
	for _, n := range molLinks {
		if _, err := os.Lstat(filepath.Join(symlinkDir, n)); !os.IsNotExist(err) {
			t.Fatalf("non-mol subcommand must not recreate %s (err=%v)", n, err)
		}
	}

	// 4. The next mol command self-heals the partial view — the core fix.
	stdout.Reset()
	stderr.Reset()
	if got := doBd([]string{"mol", "wisp", "mol-test", "--json"}, &stdout, &stderr); got != 0 {
		t.Fatalf("doBd(mol wisp) second = %d, want 0; stderr=%q", got, stderr.String())
	}
	for _, n := range molLinks {
		if _, err := os.Lstat(filepath.Join(symlinkDir, n)); err != nil {
			t.Fatalf("second mol subcommand should have re-healed %s: %v", n, err)
		}
	}
}
