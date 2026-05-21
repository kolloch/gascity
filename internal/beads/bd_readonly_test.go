package beads

import (
	"reflect"
	"testing"
)

func TestIsBdReadOnlySubcommand_KnownReads(t *testing.T) {
	reads := [][]string{
		{"list"},
		{"list", "--json"},
		{"ready", "--limit", "10"},
		{"show", "ga-sc9"},
		{"show", "ga-sc9", "--json"},
		{"count"},
		{"stats"},
		{"status"},
		{"statuses"},
		{"search", "dolt"},
		{"state"},
		{"types"},
		{"lint"},
		{"history", "ga-sc9"},
		{"diff", "abc", "def"},
		{"children", "ga-sc9"},
		{"comments", "ga-sc9"},
		{"graph", "ga-sc9"},
		{"find-duplicates"},
		{"dep", "list"},
		{"config", "get", "foo"},
		{"label", "list"},
		{"label", "ls"},
		{"label", "show", "needs:human"},
	}
	for _, args := range reads {
		if !IsBdReadOnlySubcommand(args) {
			t.Errorf("IsBdReadOnlySubcommand(%v) = false, want true", args)
		}
	}
}

func TestIsBdReadOnlySubcommand_KnownWrites(t *testing.T) {
	writes := [][]string{
		{"create"},
		{"update", "ga-sc9", "--status", "open"},
		{"close", "ga-sc9"},
		{"reopen", "ga-sc9"},
		{"delete", "ga-sc9"},
		{"note", "ga-sc9"},
		{"tag", "ga-sc9", "foo"},
		{"assign", "ga-sc9", "alice"},
		{"priority", "ga-sc9", "1"},
		{"link", "ga-sc9", "ga-c9m"},
		{"comment", "ga-sc9", "hi"},
		{"set-state", "ga-sc9", "k=v"},
		{"supersede", "ga-sc9", "--with=ga-c9m"},
		{"promote", "ga-wisp-1"},
		{"init", "--server"},
		{"import"},
		{"backup"},
		{"purge"},
		{"q", "title"},
		// Two-part subcommands where action is a write
		{"dep", "add", "ga-sc9", "ga-c9m"},
		{"dep", "remove", "ga-sc9", "ga-c9m"},
		{"config", "set", "foo", "bar"},
	}
	for _, args := range writes {
		if IsBdReadOnlySubcommand(args) {
			t.Errorf("IsBdReadOnlySubcommand(%v) = true, want false", args)
		}
	}
}

func TestIsBdReadOnlySubcommand_Edges(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"nil", nil, false},
		{"empty", []string{}, false},
		{"leading global flag — conservative no", []string{"--json", "list"}, false},
		{"dep with no second arg — conservative no", []string{"dep"}, false},
		{"config with no second arg — conservative no", []string{"config"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsBdReadOnlySubcommand(tc.args); got != tc.want {
				t.Errorf("IsBdReadOnlySubcommand(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestPrependBdReadOnlyFlagsIfApplicable_ReadCommandGetsFlags(t *testing.T) {
	got := PrependBdReadOnlyFlagsIfApplicable("bd", []string{"list", "--json"})
	want := []string{"--readonly", "--dolt-auto-commit=off", "list", "--json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPrependBdReadOnlyFlagsIfApplicable_WriteCommandUnchanged(t *testing.T) {
	in := []string{"update", "ga-sc9", "--status", "open"}
	got := PrependBdReadOnlyFlagsIfApplicable("bd", in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("got %v, want unchanged %v", got, in)
	}
}

func TestPrependBdReadOnlyFlagsIfApplicable_NonBdUnchanged(t *testing.T) {
	in := []string{"list"}
	got := PrependBdReadOnlyFlagsIfApplicable("git", in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("got %v, want unchanged %v", got, in)
	}
}

func TestPrependBdReadOnlyFlagsIfApplicable_RespectsExplicitReadonly(t *testing.T) {
	in := []string{"--readonly", "list"}
	got := PrependBdReadOnlyFlagsIfApplicable("bd", in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("got %v, want unchanged %v", got, in)
	}
}

func TestPrependBdReadOnlyFlagsIfApplicable_RespectsExplicitAutoCommit(t *testing.T) {
	// User explicitly opted into batch mode; do not stomp.
	in := []string{"--dolt-auto-commit=batch", "list"}
	got := PrependBdReadOnlyFlagsIfApplicable("bd", in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("got %v, want unchanged %v", got, in)
	}
	in2 := []string{"--dolt-auto-commit", "on", "list"}
	got2 := PrependBdReadOnlyFlagsIfApplicable("bd", in2)
	if !reflect.DeepEqual(got2, in2) {
		t.Fatalf("got %v, want unchanged %v", got2, in2)
	}
}

func TestPrependBdReadOnlyFlagsIfApplicable_DisabledByEnv(t *testing.T) {
	cases := []string{"1", "true", "TRUE", "yes", "on"}
	for _, v := range cases {
		t.Run(v, func(t *testing.T) {
			t.Setenv(disableAutoReadOnlyEnv, v)
			in := []string{"list"}
			got := PrependBdReadOnlyFlagsIfApplicable("bd", in)
			if !reflect.DeepEqual(got, in) {
				t.Fatalf("%s=%q: got %v, want unchanged %v", disableAutoReadOnlyEnv, v, got, in)
			}
		})
	}
}

func TestPrependBdReadOnlyFlagsIfApplicable_DisableEnvFalseyDoesInject(t *testing.T) {
	cases := []string{"", "0", "false", "off", "no"}
	for _, v := range cases {
		t.Run("env="+v, func(t *testing.T) {
			t.Setenv(disableAutoReadOnlyEnv, v)
			got := PrependBdReadOnlyFlagsIfApplicable("bd", []string{"list"})
			want := []string{"--readonly", "--dolt-auto-commit=off", "list"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

func TestPrependBdReadOnlyFlagsIfApplicable_DoesNotMutateInput(t *testing.T) {
	in := []string{"list", "--json"}
	inCopy := append([]string(nil), in...)
	_ = PrependBdReadOnlyFlagsIfApplicable("bd", in)
	if !reflect.DeepEqual(in, inCopy) {
		t.Fatalf("input mutated: now %v, was %v", in, inCopy)
	}
}
