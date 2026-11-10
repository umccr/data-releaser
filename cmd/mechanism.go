package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// mechanism identifies which release mechanism a command acts on.
//
// The two mechanisms share the verbs and nothing else: htsget serves range requests
// through the authorization callout, while s3 grants a researcher account direct read
// access to a copy of the data held in this account. Each takes its own inputs, so the
// mechanism is named as part of the invocation and is never inferred from which flags
// were given — choosing the wrong one is an access-control mistake that surfaces only
// as an unexplained deny (or an unintended grant) much later.
type mechanism string

const (
	mechanismS3     mechanism = "s3"
	mechanismHtsget mechanism = "htsget"
)

// mechanismNames lists the mechanisms in the order help text and error messages should
// present them.
var mechanismNames = []string{string(mechanismS3), string(mechanismHtsget)}

// parseMechanism resolves a mechanism name, rejecting anything unrecognised.
func parseMechanism(s string) (mechanism, error) {
	switch m := mechanism(strings.TrimSpace(s)); m {
	case mechanismS3, mechanismHtsget:
		return m, nil
	default:
		return "", fmt.Errorf("unknown mechanism %q: expected one of %s",
			s, strings.Join(mechanismNames, ", "))
	}
}

// mechanismFilter records which mechanisms a read-only invocation covers. A read-only
// verb invoked bare covers both, since reporting on one mechanism only because the
// other was not named would hide access rather than mis-grant it.
type mechanismFilter struct {
	// named is the mechanism the operator asked for, or empty when they asked for none.
	named mechanism
}

// newMechanismFilter builds the filter from a read-only verb's positional arguments,
// which the command's Args validator has already checked.
func newMechanismFilter(args []string) (mechanismFilter, error) {
	if len(args) == 0 {
		return mechanismFilter{}, nil
	}
	named, err := parseMechanism(args[0])
	if err != nil {
		return mechanismFilter{}, err
	}
	return mechanismFilter{named: named}, nil
}

// covers reports whether the invocation includes the given mechanism.
func (f mechanismFilter) covers(m mechanism) bool {
	return f.named == "" || f.named == m
}

// optionalMechanismArg validates the single optional mechanism argument that the
// read-only verbs accept.
func optionalMechanismArg(_ *cobra.Command, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("expected at most one mechanism (%s), got %d arguments",
			strings.Join(mechanismNames, " or "), len(args))
	}
	if len(args) == 1 {
		if _, err := parseMechanism(args[0]); err != nil {
			return err
		}
	}
	return nil
}

// requireMechanism makes a verb that changes access refuse to act unless the
// invocation names a mechanism.
//
// A parent command with no RunE of its own is not runnable, and cobra's response to
// that is to print help and exit zero — which for a verb that was meant to grant or
// stop access reports success for having done nothing, and a script cannot tell the
// difference. So the verb carries a refusal instead: it is runnable, and both its
// argument check and its RunE return an error, giving a non-zero exit.
//
// The refusal lives on the argument check because arguments are validated before the
// persistent pre-run loads configuration. An operator who forgot the mechanism is then
// told that, rather than being told about whatever else the environment is missing.
//
// Unrecognised flags are tolerated on the verb for the same reason. Otherwise
// `share --libraries …` fails on the flag, which points at a flag that is perfectly
// valid one level down instead of at the omission that actually matters. Nothing is
// lost by parsing the verb loosely, since every invocation that reaches it is refused
// and the mechanism leaf still parses its own flags strictly.
func requireMechanism(verb *cobra.Command) {
	verb.Args = requiredMechanismArg
	verb.RunE = requiredMechanismArg
	verb.FParseErrWhitelist = cobra.FParseErrWhitelist{UnknownFlags: true}
}

// requiredMechanismArg refuses an invocation of a mechanism-required verb. It only
// ever runs when cobra failed to descend into a mechanism leaf, so reaching it means
// the mechanism was absent, misspelled, or not implemented for this verb.
func requiredMechanismArg(verb *cobra.Command, args []string) error {
	if len(args) == 0 {
		return errMechanismRequired(verb)
	}
	named, err := parseMechanism(args[0])
	if err != nil {
		return fmt.Errorf("%s: %w", verb.CommandPath(), err)
	}
	return errMechanismUnsupported(verb, named)
}

// errMechanismRequired reports that a verb was invoked without naming a mechanism. It
// names every mechanism the tool knows and every form of this verb that exists, so the
// operator does not have to discover by trial which of them they meant.
func errMechanismRequired(verb *cobra.Command) error {
	msg := fmt.Sprintf("%s requires a mechanism (%s) and never infers one from the other inputs given",
		verb.CommandPath(), strings.Join(mechanismNames, " or "))
	if forms := verbForms(verb); len(forms) > 0 {
		msg += fmt.Sprintf("; run one of: %s", strings.Join(forms, ", "))
	}
	return errors.New(msg)
}

// errMechanismUnsupported reports that a verb was given a mechanism it has no leaf for,
// either because that combination is not implemented yet or because it never will be.
// Falling back to the verb's other mechanism is never the answer: the two grant
// different access.
func errMechanismUnsupported(verb *cobra.Command, named mechanism) error {
	implemented := verbMechanisms(verb)
	if len(implemented) == 0 {
		return fmt.Errorf("%s implements no mechanism yet, including %s", verb.CommandPath(), named)
	}
	return fmt.Errorf("%s does not support the %s mechanism: %s implements %s",
		verb.CommandPath(), named, verb.Name(), strings.Join(implemented, " and "))
}

// verbMechanisms lists the mechanisms a verb has a leaf for, in the order
// mechanismNames presents them. A verb need not cover both: staging and purging are
// meaningful only where this account holds a copy of the data.
func verbMechanisms(verb *cobra.Command) []string {
	var found []string
	for _, name := range mechanismNames {
		for _, child := range verb.Commands() {
			if child.Name() == name {
				found = append(found, name)
				break
			}
		}
	}
	return found
}

// verbForms renders each mechanism leaf of a verb as the invocation that reaches it.
func verbForms(verb *cobra.Command) []string {
	names := verbMechanisms(verb)
	forms := make([]string, 0, len(names))
	for _, name := range names {
		forms = append(forms, verb.CommandPath()+" "+name)
	}
	return forms
}
