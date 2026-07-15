package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

// bdSilentFallbackExitCode is the exit code gc bd emits when bd's stderr
// shows an empty-DB JSONL auto-import that gc cannot confirm reached the
// managed Dolt server (bd may have imported into an on-disk fallback if the
// managed server was unreachable). Distinct from bd's own exits so operators
// and CI can tell this signal apart from a real bd error. Covers both the
// bd update path (gastownhall/gascity#2080) and the bd close path
// (gastownhall/gascity#2079) because both subcommands flow through doBd.
const bdSilentFallbackExitCode = 4

// bdStderrScanLimit caps how much of bd's stderr gc retains to scan for the
// silent-fallback marker. bd emits the marker pair while opening the store —
// before it runs the subcommand — so the marker, when present, always lands
// within the first chunk of stderr. Capping the retained prefix keeps memory
// bounded for bd subcommands that stream large stderr output.
const bdStderrScanLimit = 64 << 10 // 64 KiB

// headLimitedWriter retains only the first limit bytes written to it and
// discards the rest, so scanning bd's stderr for the silent-fallback marker
// never holds an unbounded copy of the stream. It always reports a full
// write so it is safe as an io.MultiWriter sink.
type headLimitedWriter struct {
	buf   []byte
	limit int
}

func (w *headLimitedWriter) Write(p []byte) (int, error) {
	if room := w.limit - len(w.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		w.buf = append(w.buf, p[:room]...)
	}
	return len(p), nil
}

func (w *headLimitedWriter) String() string { return string(w.buf) }

func newBdCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bd [bd-args...]",
		Short: "Run bd in the correct rig directory",
		Long: `Run a bd command routed to the correct rig directory.

When beads belong to a rig (not the city root), bd must run from the
rig directory to find the correct .beads database. This command resolves
the rig automatically from the --rig flag or by detecting the bead prefix
in the arguments.

All arguments after "gc bd" are forwarded to bd unchanged.

gc bd forces BD_EXPORT_AUTO=false to prevent bd's git auto-export hook
from wedging the wrapper after printing command output. If you need
auto-export behavior, invoke bd directly.`,
		Example: `  gc bd --rig my-project list
  gc bd --rig my-project create "New task"
  gc bd show my-project-abc          # auto-detects rig from bead prefix
  gc bd list --rig my-project -s open`,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			// Plumb doBd's numeric exit code through exitForCode so the
			// process exit code matches the documented contract above
			// (bdSilentFallbackExitCode = 4) and bd's own exit codes are
			// preserved. Returning errExit on any non-zero would collapse
			// every code to 1 and defeat the operator/CI signal the loud-
			// fail was meant to provide.
			return exitForCode(doBd(args, stdout, stderr))
		},
	}
	return cmd
}

var bdBeadExists = func(cityPath string, target execStoreTarget, beadID string) bool {
	store, err := openStoreAtForCity(target.ScopeRoot, cityPath)
	if err != nil {
		return false
	}
	bead, err := store.Get(beadID)
	return err == nil && strings.TrimSpace(bead.ID) != ""
}

func bdCommandEnv(cityPath string, cfg *config.City, target execStoreTarget) ([]string, error) {
	var overrides map[string]string
	var err error
	if target.ScopeKind == "rig" {
		overrides, err = bdRuntimeEnvForRigWithError(cityPath, cfg, target.ScopeRoot)
	} else {
		overrides, err = bdRuntimeEnvWithError(cityPath)
	}
	if err != nil {
		return nil, err
	}
	if target.ScopeKind != "rig" {
		overrides["GC_RIG"] = ""
		overrides["GC_RIG_ROOT"] = ""
		overrides["BEADS_DIR"] = filepath.Join(target.ScopeRoot, ".beads")
	}
	overrides["GC_STORE_ROOT"] = target.ScopeRoot
	overrides["GC_STORE_SCOPE"] = target.ScopeKind
	overrides["GC_BEADS_PREFIX"] = target.Prefix
	applyExportSuppressionEnv(overrides)
	return mergeRuntimeEnv(os.Environ(), overrides), nil
}

func warnExternalBdOverrideDrift(stderr io.Writer, cityPath string, target execStoreTarget) {
	resolved, ok, err := canonicalScopeDoltTarget(cityPath, target.ScopeRoot)
	if err != nil || !ok || !resolved.External {
		return
	}
	var drift []string
	if host := strings.TrimSpace(os.Getenv("GC_DOLT_HOST")); host != "" && host != strings.TrimSpace(resolved.Host) {
		drift = append(drift, fmt.Sprintf("GC_DOLT_HOST=%s (canonical %s)", host, strings.TrimSpace(resolved.Host)))
	}
	if port := strings.TrimSpace(os.Getenv("GC_DOLT_PORT")); port != "" && port != strings.TrimSpace(resolved.Port) {
		drift = append(drift, fmt.Sprintf("GC_DOLT_PORT=%s (canonical %s)", port, strings.TrimSpace(resolved.Port)))
	}
	if len(drift) == 0 {
		return
	}
	_, _ = fmt.Fprintf(stderr, "gc bd: warning: ignoring ambient Dolt host/port override for external target: %s\n", strings.Join(drift, ", "))
}

func doBd(args []string, stdout, stderr io.Writer) int {
	cityName, rigName, bdArgs := extractBdScopeFlags(args)

	cityPath, err := resolveBdCity(cityName)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Use the full config load path (includes pack expansion + site
	// binding overlay) so migrated rigs (path only in .gc/site.toml)
	// resolve to their bound path. A raw config.Load here would make
	// every already-migrated rig look unbound and fail the new guard
	// in resolveBdScopeTarget / bdRigScopeTarget.
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: loading config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	target, err := resolveBdScopeTarget(cfg, cityPath, rigName, bdArgs)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if provider := rawBeadsProviderForScope(target.ScopeRoot, cityPath); !providerUsesBdStoreContract(provider) {
		fmt.Fprintf(stderr, "gc bd: only supported for bd-backed beads providers (resolved %q for %s)\n", provider, target.ScopeRoot) //nolint:errcheck // best-effort stderr
		if hint := bdProviderMismatchHint(target.ScopeRoot, provider); hint != "" {
			fmt.Fprintf(stderr, "  hint: %s\n", hint) //nolint:errcheck // best-effort stderr
		}
		return 1
	}

	// Self-heal the formula symlink view before molecule subcommands so the
	// witness/refinery can pour patrol wisps (`gc bd mol wisp ...`) even when a
	// prior pack rebuild left <scope>/.beads/formulas/ partial. loadCityConfig
	// above already re-materialized the builtin pack source files for this
	// process, so re-resolving the symlinks here recovers the view without a
	// manual `gc reload`. Gated to mol — the hot CRUD/query path does not read
	// the formula symlinks. See ga-y9v.
	if bdSubcommandNeedsFormulas(bdArgs) {
		healFormulaSymlinksForScope(cfg, target, stderr)
	}

	reapStaleBdExportJSONL(target.ScopeRoot)
	warnExternalBdOverrideDrift(stderr, cityPath, target)

	bdPath, err := exec.LookPath("bd")
	if err != nil {
		fmt.Fprintln(stderr, "gc bd: bd not found in PATH") //nolint:errcheck // best-effort stderr
		return 1
	}

	// Inject bd's --readonly / --dolt-auto-commit=off ahead of known
	// read-only subcommands so dolt skips the implicit commit cycle and
	// per-command connection churn drops (ga-sc9). Pass-through writes
	// and any invocation that already specifies these flags are left as
	// the operator typed them.
	bdArgs = beads.PrependBdReadOnlyFlagsIfApplicable("bd", bdArgs)

	cmd := exec.Command(bdPath, bdArgs...)
	cmd.Dir = target.ScopeRoot
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	// Tee stderr through a bounded head buffer alongside the operator's
	// pipe so we can scan it post-exec for bd's silent-fallback-to-on-disk
	// marker. Only stderr is teed: bd writes its auto-import banner there,
	// not to stdout. See gastownhall/gascity#2080 (update path) and #2079
	// (close path) — both go through this handoff.
	stderrScan := &headLimitedWriter{limit: bdStderrScanLimit}
	cmd.Stderr = io.MultiWriter(stderr, stderrScan)
	env, err := bdCommandEnv(cityPath, cfg, target)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cmd.Env = workQueryEnvForDir(env, cmd.Dir)

	runErr := cmd.Run()

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "gc bd: %v\n", runErr) //nolint:errcheck // best-effort stderr
		return 1
	}

	// bd exited 0 — but if its stderr shows bd's empty-DB auto-import banner
	// ("auto-importing ... into empty database"), bd opened an EMPTY
	// server-mode Dolt store and imported .beads/issues.jsonl into it. Post
	// beads #3691 that banner fires only on a genuinely empty database, so
	// the auto-import itself is legitimate — bd's empty-DB recovery, which
	// commits the import on this path. The catch: gc cannot tell from stderr
	// whether bd reached the managed Dolt server or fell back to an on-disk
	// store. If it was the on-disk fallback (managed server unreachable), a
	// write in this command never reached the managed server, and managed Gas
	// City sets BD_EXPORT_AUTO=false so it won't sync later (see
	// applyExportSuppressionEnv in cmd/gc/bd_env.go).
	// Because gc cannot confirm the write landed on the managed server,
	// surface a non-zero exit for operator attention instead of a silent
	// exit 0 — without claiming, as the prior wording did, that the server
	// was definitely unreachable or the write definitely lost (ga-c6jn). One
	// check here covers the whole bd-write-persistence quad
	// (gastownhall/gascity#2079 / #2080 / #2149 / #2150) because every bd
	// subcommand routes through this handoff. A non-zero bd exit is
	// intentionally left to the block above: the existing transport-retry
	// classifier already handles the timeout+marker case, and overriding a
	// real bd exit code here would mask it.
	if bdOutputIndicatesSilentFallback(stderrScan.String()) {
		fmt.Fprintln(stderr, "gc bd: bd performed an empty-DB auto-import (imported .beads/issues.jsonl into an empty Dolt database) and exited 0. This is bd's normal empty-DB recovery, not necessarily a failure. But gc cannot tell whether bd reached the managed Dolt server or fell back to an on-disk store, so it cannot confirm a write in THIS command reached the managed server. If you expected the managed server to already hold data, verify its connectivity and confirm your write landed before relying on it. (See gastownhall/gascity#2080.)") //nolint:errcheck // best-effort stderr
		return bdSilentFallbackExitCode
	}

	return 0
}

func resolveBdCity(cityName string) (string, error) {
	if strings.TrimSpace(cityName) != "" {
		return validateCityPath(cityName)
	}
	return resolveCity()
}

// extractBdScopeFlags extracts gc-owned --city/--rig flags from the raw
// argument list and returns the requested city, rig, and remaining bd args.
// It also falls back to cobra's persistent globals for "gc --city X --rig Y bd".
func extractBdScopeFlags(args []string) (string, string, []string) {
	var cityName string
	var rigName string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--city" && i+1 < len(args):
			cityName = args[i+1]
			i++
			continue
		case strings.HasPrefix(args[i], "--city="):
			cityName = strings.TrimPrefix(args[i], "--city=")
			continue
		case args[i] == "--rig" && i+1 < len(args):
			rigName = args[i+1]
			i++
			continue
		case strings.HasPrefix(args[i], "--rig="):
			rigName = strings.TrimPrefix(args[i], "--rig=")
			continue
		}
		rest = append(rest, args[i])
	}
	if cityName == "" && cityFlag != "" {
		cityName = cityFlag
	}
	if rigName == "" && rigFlag != "" {
		rigName = rigFlag
	}
	return cityName, rigName, rest
}

// extractRigFlag extracts --rig <name> from the argument list and returns
// the rig name and remaining args. Also checks the global rigFlag set by
// cobra's persistent flag parsing (for "gc --rig foo bd list" syntax).
func extractRigFlag(args []string) (string, []string) {
	_, rigName, rest := extractBdScopeFlags(args)
	return rigName, rest
}

// extractBdDirectoryFlag returns the value of bd's -C / --directory flag from
// the bd passthrough args, or "" if the flag is absent (or trails with no
// value). It recognizes the space-separated (`-C dir`, `--directory dir`),
// equals (`-C=dir`, `--directory=dir`), and attached-short (`-Cdir`) forms
// that bd's cobra flag accepts. The flag is intentionally left in args so bd
// itself still changes directory — this only mirrors the scope decision gc
// must make before exec so the bead lands in the store -C names.
func extractBdDirectoryFlag(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-C" || arg == "--directory":
			if i+1 < len(args) {
				return args[i+1]
			}
			return ""
		case strings.HasPrefix(arg, "--directory="):
			return strings.TrimPrefix(arg, "--directory=")
		case strings.HasPrefix(arg, "-C="):
			return strings.TrimPrefix(arg, "-C=")
		case strings.HasPrefix(arg, "-C") && len(arg) > 2:
			return arg[len("-C"):]
		}
	}
	return ""
}

// resolveBdScopeTarget determines the canonical scope root for a bd command.
// Priority: explicit rig name > bead prefix auto-detection > -C/--directory rig
// match > enclosing rig > city root.
func resolveBdScopeTarget(cfg *config.City, cityPath, rigName string, args []string) (execStoreTarget, error) {
	resolveRigPaths(cityPath, cfg.Rigs)
	if rigName != "" {
		rig, ok := rigByName(cfg, rigName)
		if !ok {
			return execStoreTarget{}, fmt.Errorf("rig %q not found", rigName)
		}
		if strings.TrimSpace(rig.Path) == "" {
			return execStoreTarget{}, fmt.Errorf("rig %q is declared but has no path binding — run `gc rig add <dir> --name %s` to bind it before scoping bd commands", rig.Name, rig.Name)
		}
		return bdRigScopeTarget(cityPath, rig), nil
	}

	cityTarget := bdCityScopeTarget(cityPath, cfg)
	cityPrefix := config.EffectiveHQPrefix(cfg)
	if cityPrefix != "" {
		for _, arg := range args {
			if strings.HasPrefix(arg, "-") || beadPrefix(cfg, arg) != cityPrefix {
				continue
			}
			if bdBeadExists(cityPath, cityTarget, arg) {
				return cityTarget, nil
			}
		}
	}

	// Auto-detect from bead IDs in args, but only accept candidates that
	// actually exist in the resolved rig store. This keeps hyphenated flag
	// values and other non-ID args from silently retargeting the command.
	// Unbound rigs are skipped so we don't alias them to the city store.
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if rig, ok := bdRigForArg(cfg, arg); ok {
			if strings.TrimSpace(rig.Path) == "" {
				continue
			}
			target := bdRigScopeTarget(cityPath, rig)
			if bdBeadExists(cityPath, target, arg) {
				return target, nil
			}
		}
	}

	// Honor bd's -C / --directory flag before the enclosing-rig (cwd) fallback:
	// if it names a directory that maps to a registered rig, route there. An
	// explicit -C must win over the directory the command happens to run from,
	// otherwise `gc bd create -C <rig> ...` invoked from a polecat worktree
	// would land the bead in the worktree's rig instead of the one -C names.
	// resolveRigForDir absolutizes relative -C values against cwd, matching
	// bd's own -C semantics.
	if cdDir := extractBdDirectoryFlag(args); cdDir != "" {
		if rig, ok, err := resolveRigForDir(cfg, cityPath, cdDir); err != nil {
			return execStoreTarget{}, err
		} else if ok {
			// resolveRigForDir already skips unbound rigs, so rig.Path is
			// guaranteed non-empty here.
			return bdRigScopeTarget(cityPath, rig), nil
		}
	}

	if rig, ok, err := bdRigFromCwd(cfg, cityPath); err != nil {
		return execStoreTarget{}, err
	} else if ok {
		// resolveRigForDir already skips unbound rigs, so rig.Path is
		// guaranteed non-empty here.
		return bdRigScopeTarget(cityPath, rig), nil
	}

	return cityTarget, nil
}

func bdRigForArg(cfg *config.City, arg string) (config.Rig, bool) {
	if prefix := beadPrefix(cfg, arg); prefix != "" {
		return findRigByPrefix(cfg, prefix)
	}
	return config.Rig{}, false
}

func bdRigFromCwd(cfg *config.City, cityPath string) (config.Rig, bool, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return config.Rig{}, false, nil
	}
	return resolveRigForDir(cfg, cityPath, cwd)
}

func bdRigScopeTarget(cityPath string, rig config.Rig) execStoreTarget {
	return execStoreTarget{
		ScopeRoot: resolveStoreScopeRoot(cityPath, rig.Path),
		ScopeKind: "rig",
		Prefix:    rig.EffectivePrefix(),
		RigName:   rig.Name,
	}
}

func bdCityScopeTarget(cityPath string, cfg *config.City) execStoreTarget {
	return execStoreTarget{
		ScopeRoot: resolveStoreScopeRoot(cityPath, cityPath),
		ScopeKind: "city",
		Prefix:    config.EffectiveHQPrefix(cfg),
	}
}
