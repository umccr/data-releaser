package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestParseMechanism(t *testing.T) {
	cases := []struct {
		in      string
		want    mechanism
		wantErr bool
	}{
		{"s3", mechanismS3, false},
		{"htsget", mechanismHtsget, false},
		{" htsget ", mechanismHtsget, false},
		{"", "", true},
		{"HTSGET", "", true},
		{"lattice", "", true},
	}
	for _, c := range cases {
		got, err := parseMechanism(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseMechanism(%q): expected an error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMechanism(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseMechanism(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMechanismFilterCoverage confirms a read-only verb invoked bare spans both
// mechanisms while a named one narrows to it.
func TestMechanismFilterCoverage(t *testing.T) {
	bare, err := newMechanismFilter(nil)
	if err != nil {
		t.Fatalf("newMechanismFilter(nil): %v", err)
	}
	if !bare.covers(mechanismS3) || !bare.covers(mechanismHtsget) {
		t.Error("expected a bare invocation to cover both mechanisms")
	}

	narrowed, err := newMechanismFilter([]string{"htsget"})
	if err != nil {
		t.Fatalf("newMechanismFilter([htsget]): %v", err)
	}
	if !narrowed.covers(mechanismHtsget) {
		t.Error("expected the named mechanism to be covered")
	}
	if narrowed.covers(mechanismS3) {
		t.Error("expected the unnamed mechanism to be excluded")
	}

	if _, err := newMechanismFilter([]string{"nonsense"}); err == nil {
		t.Error("expected an unknown mechanism name to be rejected")
	}
}

func TestOptionalMechanismArg(t *testing.T) {
	cases := []struct {
		args    []string
		wantErr bool
	}{
		{nil, false},
		{[]string{"s3"}, false},
		{[]string{"htsget"}, false},
		{[]string{"warehouse"}, true},
		{[]string{"s3", "htsget"}, true},
	}
	for _, c := range cases {
		err := optionalMechanismArg(nil, c.args)
		if c.wantErr && err == nil {
			t.Errorf("optionalMechanismArg(%v): expected an error", c.args)
		}
		if !c.wantErr && err != nil {
			t.Errorf("optionalMechanismArg(%v): unexpected error: %v", c.args, err)
		}
	}
}

// mutatingVerbs are the verbs that change access and therefore must never act on an
// invocation that leaves the mechanism unnamed.
var mutatingVerbs = []string{"share", "revoke", "extend"}

// findCommand resolves a command path through the root command without executing it,
// so no configuration is loaded.
func findCommand(t *testing.T, path ...string) *cobra.Command {
	t.Helper()
	found, _, err := rootCmd.Find(path)
	if err != nil {
		t.Fatalf("resolving %v: %v", path, err)
	}
	return found
}

// runCLI drives the whole command tree the way a shell invocation does, so what is
// asserted is the behaviour an operator gets rather than one validator in isolation.
// Only invocations that are refused before configuration is loaded are safe to run
// here; anything that reaches a leaf would reach AWS.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	err := rootCmd.Execute()
	return out.String(), err
}

// TestMutatingVerbsResolveToMechanismLeaf checks that each mutating verb is a parent
// whose work lives on a mechanism child, so no invocation can act without naming the
// mechanism.
func TestMutatingVerbsResolveToMechanismLeaf(t *testing.T) {
	for _, verb := range mutatingVerbs {
		parent := findCommand(t, verb)
		if parent.Name() != verb {
			t.Fatalf("expected to resolve %q, got %q", verb, parent.Name())
		}
		// The parent runs only to refuse: the mechanism carries the work, and a parent
		// that did nothing at all would exit zero after printing help.
		if err := parent.RunE(parent, nil); err == nil {
			t.Errorf("%s should refuse to act on its own; the mechanism carries the work", verb)
		}

		leaf := findCommand(t, verb, "htsget")
		if leaf.Name() != "htsget" {
			t.Fatalf("expected %s htsget, resolved %q", verb, leaf.Name())
		}
		if !leaf.Runnable() {
			t.Errorf("%s htsget should be runnable", verb)
		}
		if leaf.Parent() != parent {
			t.Errorf("%s htsget is not a child of %s", verb, verb)
		}
	}
}

// TestHtsgetLeafFlagsAndOperands checks the existing invocation moved to the leaf
// intact: the flags are declared there rather than on the parent, and the record id
// stays positional.
func TestHtsgetLeafFlagsAndOperands(t *testing.T) {
	share := findCommand(t, "share", "htsget")
	for _, name := range []string{"libraries", "account", "expiry", "target-vpc-arn"} {
		if share.Flags().Lookup(name) == nil {
			t.Errorf("share htsget is missing the --%s flag", name)
		}
		if shareCmd.Flags().Lookup(name) != nil {
			t.Errorf("--%s should be declared on the mechanism leaf, not on the share verb", name)
		}
	}

	extend := findCommand(t, "extend", "htsget")
	if extend.Flags().Lookup("duration") == nil {
		t.Error("extend htsget is missing the --duration flag")
	}

	// Both record-keyed leaves take the id as their only positional argument.
	for _, leaf := range []*cobra.Command{extend, findCommand(t, "revoke", "htsget")} {
		if err := leaf.Args(leaf, []string{"01JQ8XABCDEF0123456789ABCD"}); err != nil {
			t.Errorf("%s should accept a single record id: %v", leaf.CommandPath(), err)
		}
		if err := leaf.Args(leaf, nil); err == nil {
			t.Errorf("%s should require a record id", leaf.CommandPath())
		}
	}
}

// TestVpcFlagsBelongToTheirOwnMechanism checks that a mechanism's VPC input is declared
// on its own leaf and nowhere else.
//
// The two forms are near-homographs for different values: htsget matches a full EC2 VPC
// ARN against the identity forwarded to its authorization callout, while a direct-read
// release compares a bare vpc- identifier inside a bucket policy condition. Neither is
// usable in the other's place, and neither fails at the point it is written — a wrong one
// denies every request hours later, with nothing to distinguish it from a missing
// permission or a passed expiry. So the leaf that consumes a form is the only place that
// accepts it, and the verb above accepts neither.
//
// Only the htsget leaf exists so far; what holds regardless is that it takes the ARN form
// and rejects the identifier form outright rather than ignoring it.
func TestVpcFlagsBelongToTheirOwnMechanism(t *testing.T) {
	const (
		vpcArnFlag = "target-vpc-arn"
		vpcIDFlag  = "target-vpc-id"
	)

	share := findCommand(t, "share", "htsget")
	if share.Flags().Lookup(vpcArnFlag) == nil {
		t.Errorf("share htsget should declare --%s: the callout is keyed on the VPC ARN", vpcArnFlag)
	}
	if share.Flags().Lookup(vpcIDFlag) != nil {
		t.Errorf("share htsget should not declare --%s: a bare vpc identifier matches no forwarded identity", vpcIDFlag)
	}

	// Neither form is declared on the verb, so neither can be given without naming the
	// mechanism that interprets it.
	for _, name := range []string{vpcArnFlag, vpcIDFlag} {
		if shareCmd.Flags().Lookup(name) != nil || shareCmd.PersistentFlags().Lookup(name) != nil {
			t.Errorf("--%s should be declared on the mechanism leaf, not on the share verb", name)
		}
	}

	// The wrong form on a leaf is an error rather than ignored input. Flag parsing happens
	// before configuration is loaded, so this reaches no further than the leaf itself.
	if _, err := runCLI(t, "share", "htsget", "--"+vpcIDFlag, "vpc-0123456789abcdef0"); err == nil {
		t.Errorf("expected share htsget to reject --%s", vpcIDFlag)
	}

	// And giving either form to the bare verb still leaves the mechanism unnamed, so the
	// invocation is refused rather than resolved from the shape of the VPC value.
	for _, args := range [][]string{
		{"share", "--" + vpcArnFlag, "arn:aws:ec2:ap-southeast-2:123456789012:vpc/vpc-0123456789abcdef0"},
		{"share", "--" + vpcIDFlag, "vpc-0123456789abcdef0"},
	} {
		_, err := runCLI(t, args...)
		if err == nil {
			t.Errorf("%v should be refused: the mechanism is unnamed", args)
			continue
		}
		if !strings.Contains(err.Error(), "mechanism") {
			t.Errorf("%v should fail on the mechanism, got: %v", args, err)
		}
	}
}

// TestExpirySyntaxIsSharedAcrossMechanisms checks the one operand rule that has to hold
// across leaves: a verb that exists for both takes its expiry the same way in each, so
// moving between mechanisms does not mean relearning the duration format. The parser is
// shared, which is what makes this true; the assertion is that both leaves declare the
// flag the parser reads, and that the leaf that grants access spells it the same way as
// the leaf that moves an expiry.
func TestExpirySyntaxIsSharedAcrossMechanisms(t *testing.T) {
	if findCommand(t, "share", "htsget").Flags().Lookup("expiry") == nil {
		t.Error("share htsget should take an optional --expiry")
	}
	if findCommand(t, "extend", "htsget").Flags().Lookup("duration") == nil {
		t.Error("extend htsget should take a --duration")
	}

	// Both accept the day shorthand and Go's own duration form, so a direct-read leaf
	// added later has one syntax to match rather than a choice to make.
	for _, form := range []string{"30d", "720h", "1d"} {
		if _, err := parseDuration(form); err != nil {
			t.Errorf("expiry syntax %q should be accepted by both mechanisms: %v", form, err)
		}
	}
}

// TestReadOnlyVerbsAcceptOptionalMechanism checks that list and validate stay directly
// runnable and take the mechanism as an optional argument rather than a child command.
func TestReadOnlyVerbsAcceptOptionalMechanism(t *testing.T) {
	for _, verb := range []string{"list", "validate"} {
		cmd := findCommand(t, verb)
		if !cmd.Runnable() {
			t.Errorf("%s should be runnable when invoked bare", verb)
		}
		if cmd.HasSubCommands() {
			t.Errorf("%s should take the mechanism as an argument, not as a sub-command", verb)
		}
		for _, args := range [][]string{nil, {"s3"}, {"htsget"}} {
			if err := cmd.Args(cmd, args); err != nil {
				t.Errorf("%s %v should be accepted: %v", verb, args, err)
			}
		}
		if err := cmd.Args(cmd, []string{"both"}); err == nil {
			t.Errorf("%s should reject an unknown mechanism name", verb)
		}
	}
}

// TestBareMutatingVerbIsRefused checks that a verb which changes access does nothing and
// fails when the invocation names no mechanism. Printing help and exiting zero — cobra's
// default for a parent command — would report success for having granted nothing.
func TestBareMutatingVerbIsRefused(t *testing.T) {
	for _, verb := range mutatingVerbs {
		_, err := runCLI(t, verb)
		if err == nil {
			t.Errorf("a bare %q should fail; it acted on nothing", verb)
			continue
		}
		for _, name := range mechanismNames {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("a bare %q should name the %s mechanism, got: %v", verb, name, err)
			}
		}
	}
}

// TestMechanismIsNotInferredFromFlags checks that inputs belonging to one mechanism do
// not select it. A flag set that only htsget accepts still leaves the mechanism unnamed,
// and guessing it is the mistake this refusal exists to prevent: an s3 release granted
// where an htsget share was meant hands over whole-object reads.
func TestMechanismIsNotInferredFromFlags(t *testing.T) {
	invocations := [][]string{
		{"share", "--libraries", "L2400001", "--account", "123456789012"},
		{"share", "--libraries=L2400001"},
		{"extend", "01JQ8XABCDEF0123456789ABCD", "--duration", "30d"},
		{"revoke", "01JQ8XABCDEF0123456789ABCD"},
	}
	for _, args := range invocations {
		_, err := runCLI(t, args...)
		if err == nil {
			t.Errorf("%v should be refused: the mechanism is unnamed", args)
			continue
		}
		// The failure must be about the missing mechanism, not about the flag or the
		// record id, so the operator is pointed at what is actually wrong.
		if !strings.Contains(err.Error(), "mechanism") {
			t.Errorf("%v should fail on the mechanism, got: %v", args, err)
		}
	}
}

// TestRequireMechanismRefusals covers the refusals against a verb built for the test, so
// the cases hold for mechanism leaves that do not exist yet.
func TestRequireMechanismRefusals(t *testing.T) {
	newVerb := func(leaves ...string) *cobra.Command {
		verb := &cobra.Command{Use: "purge"}
		for _, leaf := range leaves {
			verb.AddCommand(&cobra.Command{
				Use:  leaf,
				RunE: func(*cobra.Command, []string) error { return nil },
			})
		}
		requireMechanism(verb)
		return verb
	}

	withS3 := newVerb("s3")

	// No mechanism named: both mechanisms are reported, and so is the form that exists.
	err := withS3.Args(withS3, nil)
	if err == nil {
		t.Fatal("expected a bare invocation to be refused")
	}
	for _, name := range mechanismNames {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("expected the refusal to name %s, got: %v", name, err)
		}
	}
	if !strings.Contains(err.Error(), "purge s3") {
		t.Errorf("expected the refusal to show the form that exists, got: %v", err)
	}

	// A mechanism this verb has no leaf for is refused rather than falling back to the
	// leaf it does have.
	err = withS3.Args(withS3, []string{"htsget"})
	if err == nil {
		t.Fatal("expected an unsupported mechanism to be refused")
	}
	if !strings.Contains(err.Error(), "htsget") || !strings.Contains(err.Error(), "s3") {
		t.Errorf("expected the refusal to name both the requested and the supported mechanism, got: %v", err)
	}

	// Something that is not a mechanism name at all, such as a record id given without
	// its mechanism.
	if err := withS3.Args(withS3, []string{"01JQ8XABCDEF0123456789ABCD"}); err == nil {
		t.Error("expected an unrecognised mechanism name to be refused")
	}

	// A verb with no leaves yet still refuses rather than exiting zero.
	empty := newVerb()
	if err := empty.Args(empty, nil); err == nil {
		t.Error("expected a verb with no mechanism leaves to refuse")
	}
	if err := empty.Args(empty, []string{"s3"}); err == nil {
		t.Error("expected a verb with no mechanism leaves to refuse a named mechanism")
	}

	// The refusal is on the run path too, so it holds even if argument validation is
	// replaced later.
	if err := withS3.RunE(withS3, nil); err == nil {
		t.Error("expected the verb's run to refuse")
	}
}

// TestMechanismLeafStillParsesFlagsStrictly checks that the loose flag parsing the verb
// needs in order to report a missing mechanism does not reach the leaf, where a mistyped
// flag must still be an error rather than silently ignored input.
func TestMechanismLeafStillParsesFlagsStrictly(t *testing.T) {
	// Flag parsing happens before any configuration is loaded, so this reaches no further
	// than the leaf's own argument handling.
	if _, err := runCLI(t, "share", "htsget", "--nonsense", "x"); err == nil {
		t.Error("expected an unknown flag on a mechanism leaf to be rejected")
	}
}

// TestReadOnlyVerbsAreNotMechanismRequired checks the other half of the rule: a verb that
// only reports keeps working when bare, because covering both mechanisms hides nothing.
func TestReadOnlyVerbsAreNotMechanismRequired(t *testing.T) {
	for _, verb := range []string{"list", "validate"} {
		cmd := findCommand(t, verb)
		if err := cmd.Args(cmd, nil); err != nil {
			t.Errorf("a bare %q should be accepted and cover both mechanisms: %v", verb, err)
		}
	}
}
