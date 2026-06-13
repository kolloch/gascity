package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/config"
)

// bdSubcommandNeedsFormulas reports whether a bd invocation resolves formulas
// from <scope>/.beads/formulas/. Only molecule subcommands (`bd mol wisp`,
// `bd mol bond`, `bd mol pour`, ...) read that symlink view; the hot CRUD/query
// path (list, ready, show, update, create, ...) does not. Gating the
// formula-symlink self-heal to mol keeps the work-query loop — which fires
// many bd calls per session — free of extra filesystem work.
//
// Follows the same arg convention as beads.IsBdReadOnlySubcommand: the bd
// subcommand is the first element (a global flag ahead of the subcommand is
// not classified, which only loses the self-heal for that atypical form).
func bdSubcommandNeedsFormulas(bdArgs []string) bool {
	return len(bdArgs) > 0 && bdArgs[0] == "mol"
}

// healFormulaSymlinksForScope re-materializes the formula symlinks under
// <target.ScopeRoot>/.beads/formulas/ so a partial view self-heals on the next
// `gc bd mol` call instead of waiting for a manual `gc reload` (ga-y9v).
//
// The partial view arises when a pack rebuild momentarily removes a formula
// source file (e.g. a concurrent older/newer-binary materialization prunes it,
// or the pack dir is briefly incomplete): ResolveFormulas's broken-link cleanup
// deletes the now-dangling symlink, and because no bd-path code re-runs
// ResolveFormulas, the symlink stays gone until the next gc start/reload. The
// witness/refinery pour their patrol wisps via `gc bd mol wisp`, so that path
// must recover on its own.
//
// Correct by ordering: doBd loads the city config first, which re-materializes
// the builtin pack files for this process (the per-process refresh cache is
// cold on every gc bd invocation), so the formula sources are present on disk
// by the time this runs and ResolveFormulas can recreate any missing winner.
// Layers are selected for the exact scope bd will run in (rig layers for a rig
// scope, city layers otherwise — SearchPaths falls back to city), so the healed
// view matches the .beads/formulas/ that bd is about to read.
//
// Best-effort: a resolution error is warned but never fails the bd command. The
// invocation may not need the formula that failed to resolve, and surfacing a
// transient symlink hiccup as a hard error would regress commands that work
// today.
func healFormulaSymlinksForScope(cfg *config.City, target execStoreTarget, stderr io.Writer) {
	if cfg == nil {
		return
	}
	layers := cfg.FormulaLayers.SearchPaths(target.RigName)
	if len(layers) == 0 {
		return
	}
	if err := ResolveFormulas(target.ScopeRoot, layers); err != nil {
		fmt.Fprintf(stderr, "gc bd: refreshing formula symlinks: %v\n", err) //nolint:errcheck // best-effort self-heal
	}
}
