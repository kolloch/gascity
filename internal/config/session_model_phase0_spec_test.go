package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Named Sessions / explicit name distinct from template
// - Default work_query contract
// - Default on_boot / on_death hooks
// - Cap Accounting for mode=always named sessions
// Keep these cases unique; a prior rebase duplicated the trailing block and
// broke CI typechecking.

func TestPhase0NamedSessionConfig_ExplicitNameCreatesDistinctIdentityFromTemplate(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city.toml")
	configText := `[workspace]
name = "test-city"

[[agent]]
name = "reviewer"
start_command = "true"
max_active_sessions = 2

[[named_session]]
name = "mayor"
template = "reviewer"

[[named_session]]
name = "triage"
template = "reviewer"
`
	if err := os.WriteFile(cityPath, []byte(configText), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	cfg, err := Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("Load(city.toml): %v", err)
	}
	if len(cfg.NamedSessions) != 2 {
		t.Fatalf("len(NamedSessions) = %d, want 2", len(cfg.NamedSessions))
	}
	if got := cfg.NamedSessions[0].QualifiedName(); got != "mayor" {
		t.Fatalf("first QualifiedName = %q, want mayor", got)
	}
	if got := cfg.NamedSessions[1].QualifiedName(); got != "triage" {
		t.Fatalf("second QualifiedName = %q, want triage", got)
	}
	if got := cfg.NamedSessions[0].Template; got != "reviewer" {
		t.Fatalf("first Template = %q, want reviewer", got)
	}
	if got := cfg.NamedSessions[1].Template; got != "reviewer" {
		t.Fatalf("second Template = %q, want reviewer", got)
	}
	if FindNamedSession(cfg, "mayor") == nil {
		t.Fatal("FindNamedSession(cfg, mayor) = nil, want named identity mayor")
	}
	if FindNamedSession(cfg, "triage") == nil {
		t.Fatal("FindNamedSession(cfg, triage) = nil, want named identity triage")
	}
	if FindAgent(cfg, "reviewer") == nil {
		t.Fatal("FindAgent(cfg, reviewer) = nil, want backing config reviewer")
	}
}

func TestPhase0ConfigDefaults_WorkQueryIsOriginAware(t *testing.T) {
	a := Agent{Name: "worker", Dir: "myrig"}

	got := a.EffectiveWorkQuery()

	if !strings.Contains(got, "GC_SESSION_ORIGIN") {
		t.Fatalf("EffectiveWorkQuery() = %q, want origin-aware GC_SESSION_ORIGIN branch", got)
	}
	if !strings.Contains(got, "ephemeral") {
		t.Fatalf("EffectiveWorkQuery() = %q, want origin-specific ephemeral generic queue tier", got)
	}
	if !strings.Contains(got, "gc.routed_to=myrig/worker") {
		t.Fatalf("EffectiveWorkQuery() = %q, want qualified config route", got)
	}
}

func TestPhase0ConfigDefaults_OnBootUnclaimsRoutedWorkByDefault(t *testing.T) {
	a := Agent{Name: "worker", Dir: "myrig"}

	got := a.EffectiveOnBoot()
	for _, want := range []string{
		"bd list --metadata-field gc.routed_to=myrig/worker",
		"--status=in_progress",
		"--no-assignee",
		"--status open",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("EffectiveOnBoot() = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, `--assignee ""`) {
		t.Fatalf("EffectiveOnBoot() = %q, want to target only ownerless work instead of bulk-unassigning routed work", got)
	}
}

func TestPhase0ConfigDefaults_OnDeathUnclaimsAssignedWorkByDefault(t *testing.T) {
	a := Agent{Name: "worker", Dir: "myrig"}

	got := a.EffectiveOnDeath()
	for _, want := range []string{
		"bd list --assignee=myrig/worker",
		"--status=in_progress",
		`--assignee "$current_route" --status open`,
		`--assignee "myrig/worker" --status open`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("EffectiveOnDeath() = %q, want %q", got, want)
		}
	}
	// ga-wv45: OnDeath re-parks assigned work on its route for re-claim
	// instead of clearing the assignee. A cleared assignee strands
	// named-session-routed work, since the work_query's Tier 3 routed queue
	// only re-claims for ephemeral sessions.
	if strings.Contains(got, `--assignee "" `) {
		t.Fatalf("EffectiveOnDeath() = %q, must not clear assignee to empty", got)
	}
}

func TestPhase0ConfigDefaults_OnDeathClearsPoolAssigneeByDefault(t *testing.T) {
	// A pool instance (PoolName set, as stamped by the controller's pool
	// expansion) clears the assignee for its OWN route on death, so an
	// ephemeral replacement re-claims via Tier 3a (routed + --unassigned) and
	// the --claim CAS. Re-parking on the pool name yields a
	// discoverable-but-unclaimable bead the scaler perpetually re-counts as
	// demand (ga-k6re). Named-session (foreign) routes stay re-parked.
	a := Agent{Name: "worker-1", Dir: "myrig", PoolName: "myrig/worker"}

	got := a.EffectiveOnDeath()
	for _, want := range []string{
		"bd list --assignee=myrig/worker-1",
		"--status=in_progress",
		`elif [ "$current_route" = "myrig/worker" ]`,
		`--assignee "" --status open`,
		"--set-metadata gc.routed_to=myrig/worker",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("EffectiveOnDeath() = %q, want %q", got, want)
		}
	}
	// Must never re-park pool work on the unclaimable pool name (the ga-k6re
	// wedge).
	if strings.Contains(got, `--assignee "myrig/worker" --status open`) {
		t.Fatalf("EffectiveOnDeath() = %q, must not re-park pool work on the unclaimable pool name", got)
	}
}

func TestPhase0NamedSessionConfig_DuplicateExplicitNamesRejectedAcrossTemplates(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city.toml")
	configText := `[workspace]
name = "test-city"

[[agent]]
name = "reviewer"
start_command = "true"

[[agent]]
name = "coder"
start_command = "true"

[[named_session]]
name = "mayor"
template = "reviewer"

[[named_session]]
name = "mayor"
template = "coder"
`
	if err := os.WriteFile(cityPath, []byte(configText), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	if _, err := Load(fsys.OSFS{}, cityPath); err == nil {
		t.Fatal("Load(city.toml) error = nil, want duplicate configured named-session identity rejection")
	}
}

func TestPhase0NamedSessionConfig_AlwaysModeCannotExceedBackingConfigCapacity(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city.toml")
	configText := `[workspace]
name = "test-city"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1

[[named_session]]
name = "one"
template = "worker"
mode = "always"

[[named_session]]
name = "two"
template = "worker"
mode = "always"
`
	if err := os.WriteFile(cityPath, []byte(configText), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	_, err := Load(fsys.OSFS{}, cityPath)
	if err == nil {
		t.Fatal("Load(city.toml) error = nil, want mode=always named-session capacity rejection")
	}
	if !strings.Contains(err.Error(), "max_active_sessions") && !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("Load(city.toml) error = %v, want explicit capacity/max_active_sessions rejection", err)
	}
}

func TestPhase0NamedSessionConfig_OmittedNameDefaultsToTemplateIdentity(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city.toml")
	configText := `[workspace]
name = "test-city"

[[agent]]
name = "reviewer"
start_command = "true"

[[named_session]]
template = "reviewer"
`
	if err := os.WriteFile(cityPath, []byte(configText), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	cfg, err := Load(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("Load(city.toml): %v", err)
	}
	if len(cfg.NamedSessions) != 1 {
		t.Fatalf("len(NamedSessions) = %d, want 1", len(cfg.NamedSessions))
	}
	if got := cfg.NamedSessions[0].QualifiedName(); got != "reviewer" {
		t.Fatalf("QualifiedName = %q, want compatibility default reviewer", got)
	}
}
