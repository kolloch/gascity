package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestDoctorSkipsDoltChecksTreatsExecGcBeadsBdAsBdContract(t *testing.T) {
	cityDir := t.TempDir()
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(cityDir))
	if doctorSkipsDoltChecks(cityDir) {
		t.Fatal("doctorSkipsDoltChecks() = true, want false for exec:gc-beads-bd")
	}
}

func TestDoctorSkipsDoltChecksDetectsBdRigUnderFileBackedCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if doctorSkipsDoltChecks(cityDir) {
		t.Fatal("doctorSkipsDoltChecks() = true, want false for bd-backed rig")
	}
}

func TestManagedDoltOpsCheckSkipKeepsCityManagedWorkspaceEnabled(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Rigs: nil}
	if managedDoltOpsCheckSkip(cityDir, cfg, nil) {
		t.Fatal("managedDoltOpsCheckSkip() = true, want false for city-managed workspace without rigs")
	}
}

func TestManagedDoltOpsCheckSkipOnConfigError(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !managedDoltOpsCheckSkip(cityDir, nil, os.ErrInvalid) {
		t.Fatal("managedDoltOpsCheckSkip() = false, want true when city config failed to load")
	}
}

func TestManagedDoltOpsCheckUsesDoctorApplicabilityOnConfigError(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if managedDoltOpsCheckSkip(cityDir, nil, os.ErrInvalid) {
		t.Fatal("managedDoltOpsCheckSkip() = true, want false when broken city still has managed bd metadata")
	}
	if !doctor.ManagedLocalDoltChecksApplicableForConfig(cityDir, nil, os.ErrInvalid) {
		t.Fatal("doctor applicability = false, want true for same broken managed city")
	}
}

func TestManagedDoltOpsCheckDiscoversRigMetadataOnConfigError(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if managedDoltOpsCheckSkip(cityDir, nil, os.ErrInvalid) {
		t.Fatal("managedDoltOpsCheckSkip() = true, want false when broken city still has managed rig metadata")
	}
}

func TestDoDoctorRunsCityDoltCheckForInheritedBdRigUnderFileBackedCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalConfig(fsys.OSFS{}, filepath.Join(rigDir, ".beads", "config.yaml"), contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(rigDir, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: "fe",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY_PATH", cityDir)

	oldCityCheck := newDoctorDoltServerCheck
	oldRigCheck := newDoctorRigDoltServerCheck
	var citySkip, rigSkip *bool
	newDoctorDoltServerCheck = func(cityPath string, skip bool) *doctor.DoltServerCheck {
		citySkip = &skip
		return doctor.NewDoltServerCheck(cityPath, true)
	}
	newDoctorRigDoltServerCheck = func(cityPath string, rig config.Rig, skip bool) *doctor.RigDoltServerCheck {
		rigSkip = &skip
		return doctor.NewRigDoltServerCheck(cityPath, rig, true)
	}
	t.Cleanup(func() {
		newDoctorDoltServerCheck = oldCityCheck
		newDoctorRigDoltServerCheck = oldRigCheck
	})

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, false, false, &stdout, &stderr)

	if citySkip == nil || *citySkip {
		t.Fatalf("city dolt check skip = %v, want false when a bd-backed rig inherits the city endpoint", citySkip)
	}
	if rigSkip == nil || *rigSkip {
		t.Fatalf("rig dolt check skip = %v, want false for bd-backed rig", rigSkip)
	}
}

func TestDoDoctorRunsDoltTopologyForBdRigUnderFileBackedCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
dolt_host = "rig.example.com"
dolt_port = "3308"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCanonicalScopeConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(rigDir, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: "fe",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY_PATH", cityDir)

	oldCityCheck := newDoctorDoltServerCheck
	oldRigCheck := newDoctorRigDoltServerCheck
	newDoctorDoltServerCheck = func(cityPath string, _ bool) *doctor.DoltServerCheck {
		return doctor.NewDoltServerCheck(cityPath, true)
	}
	newDoctorRigDoltServerCheck = func(cityPath string, rig config.Rig, _ bool) *doctor.RigDoltServerCheck {
		return doctor.NewRigDoltServerCheck(cityPath, rig, true)
	}
	t.Cleanup(func() {
		newDoctorDoltServerCheck = oldCityCheck
		newDoctorRigDoltServerCheck = oldRigCheck
	})

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, false, false, &stdout, &stderr)

	if !strings.Contains(stdout.String(), "canonical/compat Dolt drift") {
		t.Fatalf("doctor output missing Dolt topology drift:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
}

func TestDoDoctorRegistersStaleLocalPackDirCheck(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "packs", "actual"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[packs.actual]
source = "https://github.com/gastownhall/gc-actual-packs"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_DOLT", "skip")
	cleanupManagedDoltTestCity(t, cityDir)

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, false, &stdout, &stderr)
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "stale-local-pack-dirs") {
		t.Fatalf("doctor output missing stale-local-pack-dirs check:\n%s", out)
	}
	if !strings.Contains(out, "delete `packs/actual/` (it's stale); edits go via PR on gc-actual-packs") {
		t.Fatalf("doctor output missing stale pack action:\n%s", out)
	}
}

func TestDoDoctorRegistersStaleLocalPackDirCheckForRemoteImport(t *testing.T) {
	cityDir := t.TempDir()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	source := "https://github.com/gastownhall/gc-actual-packs"
	commit := writeDoctorRemotePackFixture(t, homeDir, source)

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "packs", "actual"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[imports.actual]
source = "https://github.com/gastownhall/gc-actual-packs"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDoctorPackLock(t, cityDir, source, commit)

	out := runDoctorForStaleLocalPackDirTest(t, cityDir)
	if !strings.Contains(out, "stale-local-pack-dirs") {
		t.Fatalf("doctor output missing stale-local-pack-dirs check:\n%s", out)
	}
	if !strings.Contains(out, "packs/actual exists while [imports.actual] points at https://github.com/gastownhall/gc-actual-packs") {
		t.Fatalf("doctor output missing remote import stale pack detail:\n%s", out)
	}
}

func TestDoDoctorRegistersStaleLocalPackDirCheckForRigRemoteImport(t *testing.T) {
	cityDir := t.TempDir()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	source := "https://github.com/gastownhall/gc-actual-packs"
	commit := writeDoctorRemotePackFixture(t, homeDir, source)

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "packs", "actual"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "demo-rig"
path = "rig"

[rigs.imports.actual]
source = "https://github.com/gastownhall/gc-actual-packs"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDoctorPackLock(t, cityDir, source, commit)

	out := runDoctorForStaleLocalPackDirTest(t, cityDir)
	if !strings.Contains(out, "stale-local-pack-dirs") {
		t.Fatalf("doctor output missing stale-local-pack-dirs check:\n%s", out)
	}
	if !strings.Contains(out, "packs/actual exists while [rigs.demo-rig.imports.actual] points at https://github.com/gastownhall/gc-actual-packs") {
		t.Fatalf("doctor output missing rig remote import stale pack detail:\n%s", out)
	}
}

func TestDoDoctorRegistersStaleLocalPackDirCheckForDefaultRigRemoteImport(t *testing.T) {
	cityDir := t.TempDir()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	source := "https://github.com/gastownhall/gc-actual-packs"
	commit := writeDoctorRemotePackFixture(t, homeDir, source)

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "packs", "actual"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte(`[pack]
name = "demo"
schema = 2

[defaults.rig.imports.actual]
source = "https://github.com/gastownhall/gc-actual-packs"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDoctorPackLock(t, cityDir, source, commit)

	out := runDoctorForStaleLocalPackDirTest(t, cityDir)
	if !strings.Contains(out, "stale-local-pack-dirs") {
		t.Fatalf("doctor output missing stale-local-pack-dirs check:\n%s", out)
	}
	if !strings.Contains(out, "packs/actual exists while [defaults.rig.imports.actual] points at https://github.com/gastownhall/gc-actual-packs") {
		t.Fatalf("doctor output missing default rig remote import stale pack detail:\n%s", out)
	}
}

func writeDoctorRemotePackFixture(t *testing.T, homeDir, source string) string {
	t.Helper()

	repoDir := filepath.Join(t.TempDir(), "remote-pack")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "pack.toml"), []byte(`[pack]
name = "actual"
schema = 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitImport(t, repoDir, "init")
	mustGitImport(t, repoDir, "add", ".")
	mustGitImport(t, repoDir, "commit", "-m", "initial")
	commit := gitOutputImport(t, repoDir, "rev-parse", "HEAD")
	cacheDir := filepath.Join(homeDir, ".gc", "cache", "repos", config.RepoCacheKey(source, commit))
	if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(repoDir, cacheDir); err != nil {
		t.Fatal(err)
	}
	return commit
}

func writeDoctorPackLock(t *testing.T, cityDir, source, commit string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(cityDir, "packs.lock"), []byte(`schema = 1

[packs."`+source+`"]
version = "1.0.0"
commit = "`+commit+`"
fetched = "2026-05-20T00:00:00Z"
`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runDoctorForStaleLocalPackDirTest(t *testing.T, cityDir string) string {
	t.Helper()

	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_DOLT", "skip")
	cleanupManagedDoltTestCity(t, cityDir)

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, false, &stdout, &stderr)
	return stdout.String() + stderr.String()
}

func TestDoDoctorReportsLegacyBDSplitStore(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{
		filepath.Join(cityDir, ".beads", "dolt", "hq", ".dolt"),
		filepath.Join(cityDir, ".beads", "embeddeddolt", "legacy", ".dolt"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("GC_BEADS", "file")
	origCityFlag := cityFlag
	cityFlag = cityDir
	t.Cleanup(func() { cityFlag = origCityFlag })

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, false, false, &stdout, &stderr)
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "bd-split-store") {
		t.Fatalf("doctor output missing bd-split-store check:\n%s", out)
	}
	if !strings.Contains(out, "legacy split store") {
		t.Fatalf("doctor output missing split-store warning:\n%s", out)
	}
}

func TestCollectPackDirsEmpty(t *testing.T) {
	cfg := &config.City{}
	dirs := collectPackDirs(cfg)
	if len(dirs) != 0 {
		t.Errorf("expected no dirs, got %v", dirs)
	}
}

func TestCollectPackDirsCityLevel(t *testing.T) {
	cfg := &config.City{
		PackDirs: []string{"/a", "/b"},
	}
	dirs := collectPackDirs(cfg)
	if len(dirs) != 2 {
		t.Fatalf("expected 2 dirs, got %d: %v", len(dirs), dirs)
	}
	if dirs[0] != "/a" || dirs[1] != "/b" {
		t.Errorf("dirs = %v, want [/a /b]", dirs)
	}
}

func TestCollectPackDirsRigLevel(t *testing.T) {
	cfg := &config.City{
		RigPackDirs: map[string][]string{
			"rig1": {"/x", "/y"},
			"rig2": {"/z"},
		},
	}
	dirs := collectPackDirs(cfg)
	if len(dirs) != 3 {
		t.Fatalf("expected 3 dirs, got %d: %v", len(dirs), dirs)
	}
}

func TestCollectPackDirsDeduplicates(t *testing.T) {
	cfg := &config.City{
		PackDirs: []string{"/shared", "/a"},
		RigPackDirs: map[string][]string{
			"rig1": {"/shared", "/b"}, // /shared is a duplicate
		},
	}
	dirs := collectPackDirs(cfg)
	// /shared should appear only once.
	count := 0
	for _, d := range dirs {
		if d == "/shared" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("/shared appears %d times, want 1", count)
	}
	if len(dirs) != 3 {
		t.Fatalf("expected 3 unique dirs, got %d: %v", len(dirs), dirs)
	}
}

func TestCollectPackDirsMixed(t *testing.T) {
	cfg := &config.City{
		PackDirs: []string{"/city-topo"},
		RigPackDirs: map[string][]string{
			"rig1": {"/rig-topo"},
		},
	}
	dirs := collectPackDirs(cfg)
	if len(dirs) != 2 {
		t.Fatalf("expected 2 dirs, got %d: %v", len(dirs), dirs)
	}
}

func TestDoctorStoreFactoryUsesExplicitCityForRigOutsideCityTree(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	captureDir := t.TempDir()
	script := writeExecCaptureScript(t, captureDir)
	writeExecStoreCityConfig(t, cityDir, "metro-city", "ct", []config.Rig{{
		Name:   "frontend",
		Path:   rigDir,
		Prefix: "fe",
	}})
	t.Setenv("GC_BEADS", "exec:"+script)

	store, err := openStoreForCity(cityDir)(rigDir)
	if err != nil {
		t.Fatalf("openStoreForCity(rig): %v", err)
	}
	if _, err := store.Create(beads.Bead{Title: "rig"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rigEnv := readExecCaptureEnv(t, filepath.Join(captureDir, "frontend.env"))
	if got := rigEnv["GC_CITY_PATH"]; got != cityDir {
		t.Fatalf("GC_CITY_PATH = %q, want %q", got, cityDir)
	}
	if got := rigEnv["GC_STORE_ROOT"]; got != rigDir {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", got, rigDir)
	}
	if got := rigEnv["GC_BEADS_PREFIX"]; got != "fe" {
		t.Fatalf("GC_BEADS_PREFIX = %q, want fe", got)
	}
	if got := rigEnv["GC_RIG"]; got != "frontend" {
		t.Fatalf("GC_RIG = %q, want frontend", got)
	}
}

func TestDoctorStoreFactoryLegacyFileRigUsesSharedCityStoreWithoutCreatingRigState(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyCityStore, err := openScopeLocalFileStore(cityDir)
	if err != nil {
		t.Fatalf("openScopeLocalFileStore(city): %v", err)
	}
	if _, err := legacyCityStore.Create(beads.Bead{Title: "legacy city bead", Type: "task"}); err != nil {
		t.Fatalf("legacy city Create: %v", err)
	}
	store, err := openStoreForCity(cityDir)(rigDir)
	if err != nil {
		t.Fatalf("openStoreForCity(rig): %v", err)
	}
	list, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("rig List: %v", err)
	}
	if len(list) != 1 || list[0].Title != "legacy city bead" {
		t.Fatalf("rig store should read legacy shared city data, got %#v", list)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".gc")); !os.IsNotExist(err) {
		t.Fatalf("doctor store factory should not create rig .gc state, stat err = %v", err)
	}
}

func TestDoctorSkipsSuspendedRigChecks(t *testing.T) {
	t.Parallel()
	activeDir := t.TempDir()
	suspendedDir := t.TempDir()

	rigs := []config.Rig{
		{Name: "active-rig", Path: activeDir},
		{Name: "suspended-rig", Path: suspendedDir, Suspended: true},
	}

	// Mirror the per-rig registration logic from doDoctor.
	d := &doctor.Doctor{}
	for _, rig := range rigs {
		if rig.Suspended {
			continue
		}
		d.Register(doctor.NewRigPathCheck(rig))
	}

	var buf bytes.Buffer
	ctx := &doctor.CheckContext{CityPath: t.TempDir()}
	d.Run(ctx, &buf, false)

	out := buf.String()
	if !strings.Contains(out, "active-rig") {
		t.Error("expected active-rig checks to be registered")
	}
	if strings.Contains(out, "suspended-rig") {
		t.Error("suspended-rig checks should not be registered")
	}
}

func TestDoltTopologyCheckReportsCanonicalCompatCityDrift(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"

[dolt]
host = "city.example.com"
port = 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCanonicalScopeConfig(t, cityDir, contract.ConfigState{
		IssuePrefix:    "hq",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	res := newDoltTopologyCheck(cityDir, cfg).Run(&doctor.CheckContext{CityPath: cityDir})
	if res.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error", res.Status)
	}
	if !strings.Contains(res.Message, "deprecated city.toml [dolt] endpoint conflicts") {
		t.Fatalf("message = %q, want city drift", res.Message)
	}
	if res.FixHint == "" {
		t.Fatal("expected fix hint for topology drift")
	}
}

func TestDoltTopologyCheckReportsInheritedRigCompatDrift(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
dolt_host = "rig.example.com"
dolt_port = "3308"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCanonicalScopeConfig(t, cityDir, contract.ConfigState{
		IssuePrefix:    "hq",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeCanonicalScopeConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	resolveRigPaths(cityDir, cfg.Rigs)

	res := newDoltTopologyCheck(cityDir, cfg).Run(&doctor.CheckContext{CityPath: cityDir})
	if res.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error", res.Status)
	}
	if !strings.Contains(res.Message, `deprecated rig dolt_host/dolt_port conflict with inherited canonical endpoint for rig "frontend"`) {
		t.Fatalf("message = %q, want inherited rig drift", res.Message)
	}
}

func TestDoltTopologyCheckAllowsInheritedRigCompatMirrorForExternalCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"

[dolt]
host = "city.example.com"
port = 3307

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
dolt_host = "city.example.com"
dolt_port = "3307"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCanonicalScopeConfig(t, cityDir, contract.ConfigState{
		IssuePrefix:    "hq",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "city.example.com",
		DoltPort:       "3307",
	})
	writeCanonicalScopeConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "city.example.com",
		DoltPort:       "3307",
	})
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	resolveRigPaths(cityDir, cfg.Rigs)

	res := newDoltTopologyCheck(cityDir, cfg).Run(&doctor.CheckContext{CityPath: cityDir})
	if res.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok; message = %q", res.Status, res.Message)
	}
}
