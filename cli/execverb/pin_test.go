package execverb

import (
	"strings"
	"testing"
)

// umbra#6821: Kai wanted --type SecureString fixed on ssm put-parameter, so a
// caller could not pass anything else and need not type it.
func putParameterGrant() Grant {
	return Grant{
		Subcommand: []string{"ssm", "put-parameter"},
		Pins:       []FlagPin{{Flag: "--type", Value: "SecureString"}},
		ValueFlags: []string{"--type"},
	}
}

func TestPinSuppliesTheFlagTheCallerOmitted(t *testing.T) {
	got, err := applyPins([]string{"--name", "/x/y"}, putParameterGrant())
	if err != nil {
		t.Fatalf("applyPins: %v", err)
	}
	if v, ok := flagValue(got, "--type"); !ok || v != "SecureString" {
		t.Errorf("argv = %v, want --type SecureString supplied", got)
	}
}

// Correctness by construction is the point: nothing to type, so the guardfile
// stops depending on every caller remembering.
func TestPinRefusesAConflictingValue(t *testing.T) {
	_, err := applyPins([]string{"--name", "/x/y", "--type", "String"}, putParameterGrant())
	if err == nil {
		t.Fatal("a caller passing a different value must be refused, not silently overridden")
	}
	for _, want := range []string{"--type", "SecureString", "String"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q so the caller can see the conflict: %v", want, err)
		}
	}
}

// Passing the pinned value is not a conflict, so a caller who spells it out is
// not punished for agreeing with the policy.
func TestPinAcceptsTheMatchingValue(t *testing.T) {
	for _, args := range [][]string{
		{"--type", "SecureString"},
		{"--type=SecureString"},
	} {
		got, err := applyPins(args, putParameterGrant())
		if err != nil {
			t.Errorf("%v: %v", args, err)
			continue
		}
		if len(got) != len(args) {
			t.Errorf("%v: pin duplicated an already-correct flag: %v", args, got)
		}
	}
}

func TestPinLeavesAnUnpinnedGrantAlone(t *testing.T) {
	g := Grant{Subcommand: []string{"ssm", "get-parameter"}}
	got, err := applyPins([]string{"--name", "/x/y"}, g)
	if err != nil || len(got) != 2 {
		t.Errorf("got %v, err %v; an unpinned grant must be untouched", got, err)
	}
}

func TestParsePinRejectsAMalformedClause(t *testing.T) {
	cases := map[string][]string{
		"one argument":    {"--type"},
		"three arguments": {"--type", "SecureString", "extra"},
		"short flag":      {"-t", "SecureString"},
		"bare word":       {"type", "SecureString"},
		"empty value":     {"--type", ""},
	}
	for name, values := range cases {
		g := &Grant{Subcommand: []string{"x"}}
		if err := parsePin(g, values); err == nil {
			t.Errorf("%s must fail closed", name)
		}
	}
}

func TestParsePinRefusesADuplicateFlag(t *testing.T) {
	g := &Grant{Subcommand: []string{"x"}}
	if err := parsePin(g, []string{"--type", "SecureString"}); err != nil {
		t.Fatal(err)
	}
	if err := parsePin(g, []string{"--type", "String"}); err == nil {
		t.Fatal("a flag has one pinned value; the second must be refused rather than silently winning")
	}
}

// A pin implies value-flag, so an argN guard beside it does not read the
// pinned value as a positional.
func TestParsePinDeclaresTheFlagsArity(t *testing.T) {
	g := &Grant{Subcommand: []string{"x"}}
	if err := parsePin(g, []string{"--type", "SecureString"}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range g.ValueFlags {
		if f == "--type" {
			found = true
		}
	}
	if !found {
		t.Error("pin must declare the flag as value-taking, else positionals(argv) reads SecureString as an arg")
	}
}

// A repeat is read by the wrapped tool as its last value, so the pin has to
// hold for every occurrence and not only the first (COI-2977).
func TestPinRefusesAConflictingRepeat(t *testing.T) {
	for _, argv := range [][]string{
		{"--type", "SecureString", "--type", "String"},
		{"--type=SecureString", "--type", "String"},
		{"--type", "String", "--type", "SecureString"},
	} {
		if _, err := applyPins(argv, putParameterGrant()); err == nil {
			t.Errorf("%v: a conflicting occurrence must be refused", argv)
		}
	}
	if _, err := applyPins([]string{"--type", "SecureString", "--type=SecureString"}, putParameterGrant()); err != nil {
		t.Errorf("repeating the pinned value agrees with the policy: %v", err)
	}
}
