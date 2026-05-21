package beads

import (
	"os"
	"strings"
)

// disableAutoReadOnlyEnv opts a process out of the gc-side auto-injection
// of bd's --readonly / --dolt-auto-commit=off global flags. Useful when
// a future bd release changes the semantics of those flags or when an
// operator wants to compare behavior with and without the optimization.
const disableAutoReadOnlyEnv = "GC_BD_NO_AUTO_READONLY"

// IsBdReadOnlySubcommand reports whether args describes a known bd
// subcommand that does not modify the beads database. Args is the
// argv passed to bd (subcommand first, then flags and positionals;
// global flags ahead of the subcommand are not supported by this
// classifier and yield false, which only loses an optimization).
//
// The classifier is a conservative whitelist: any subcommand not on
// the list is treated as MUTATING. False negatives just lose the
// read-only optimization; false positives would cause bd to reject
// a legitimate write when invoked with --readonly.
func IsBdReadOnlySubcommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	sub := args[0]
	if strings.HasPrefix(sub, "-") {
		return false
	}

	switch sub {
	case "count",
		"list",
		"ready",
		"show",
		"stats",
		"status",
		"statuses",
		"search",
		"state",
		"types",
		"lint",
		"history",
		"diff",
		"children",
		"comments",
		"graph",
		"find-duplicates":
		return true
	case "dep":
		return len(args) >= 2 && args[1] == "list"
	case "config":
		return len(args) >= 2 && args[1] == "get"
	case "label":
		return len(args) >= 2 && (args[1] == "list" || args[1] == "ls" || args[1] == "show")
	}
	return false
}

// PrependBdReadOnlyFlagsIfApplicable returns args with bd's --readonly
// and --dolt-auto-commit=off global flags prepended when name is "bd"
// and the invocation is a known read-only subcommand. Args is returned
// unchanged when:
//
//   - name is not "bd"
//   - the subcommand is not in the conservative read-only whitelist
//   - the caller already specified --readonly or --dolt-auto-commit
//     (respecting explicit operator intent)
//   - the disableAutoReadOnlyEnv environment variable is truthy
//
// Prepending (rather than appending) places the flags ahead of the
// subcommand where bd parses globals, matching the existing pattern
// in cmd/gc/dispatch_runtime.go's hand-built `bd --readonly ready` calls.
func PrependBdReadOnlyFlagsIfApplicable(name string, args []string) []string {
	if name != "bd" {
		return args
	}
	if bdAutoReadOnlyDisabled() {
		return args
	}
	if hasBdGlobalFlag(args, "--readonly") || hasBdGlobalFlag(args, "--dolt-auto-commit") {
		return args
	}
	if !IsBdReadOnlySubcommand(args) {
		return args
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, "--readonly", "--dolt-auto-commit=off")
	out = append(out, args...)
	return out
}

func bdAutoReadOnlyDisabled() bool {
	v := strings.TrimSpace(os.Getenv(disableAutoReadOnlyEnv))
	if v == "" {
		return false
	}
	switch strings.ToLower(v) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

func hasBdGlobalFlag(args []string, name string) bool {
	prefix := name + "="
	for _, a := range args {
		if a == name || strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}
