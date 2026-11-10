package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/umccr/data-releaser/internal/config"
)

var (
	cfgFile string
	profile string
	appCfg  *config.Config
)

var rootCmd = &cobra.Command{
	Use:   "data-releaser",
	Short: "Grant external researchers access to bioinformatics data, by either release mechanism",
	Long: `Data Releaser grants external researchers access to bioinformatics data
held by UMCCR, through one of two mechanisms that share these verbs and
nothing else.

The htsget mechanism serves range requests for libraries through htsget-rs,
with each request authorised by a callout that reads the share record; access
is bounded by VPC Lattice connectivity from the researcher's own VPC.

The s3 mechanism copies a whole dataset into a bucket in this account and
grants a researcher account direct read access to it from one nominated VPC.
Access there is enforced by the bucket's policy and the policy of the key
encrypting it, evaluated by AWS on every request, with nothing of ours in the
read path.

The mechanism is named as part of the invocation and is never inferred from
which flags were given: a verb that changes access refuses to act without it,
since the two grant quite different access and the mistake surfaces only much
later. A read-only verb invoked without one covers both.`,
	// Execute renders the error itself, once. Without these, cobra also prints it and
	// appends a full usage dump, so an operational failure (bad credentials, missing
	// table) was reported two or three times and buried under flag documentation.
	SilenceErrors: true,
	SilenceUsage:  true,
	// Configuration is read here and checked nowhere here. What a command needs depends
	// on which mechanism it acts through, and the two share a file but almost no fields,
	// so a check applied to every command would refuse to start one mechanism's command
	// over the other mechanism's missing configuration. Each command declares its own
	// sections through requireConfig instead.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load(cfgFile)
		if err != nil {
			return err
		}

		// Apply CLI flag overrides
		overrides := config.FlagOverrides{
			Profile: profile,
		}
		cfg.ApplyFlagOverrides(overrides)

		appCfg = cfg
		return nil
	},
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: ./config.yaml or ~/.config/data-releaser/config.yaml)")
	rootCmd.PersistentFlags().StringVar(&profile, "profile", "", "AWS profile override (overrides config file profile)")
}

// requireConfig returns the loaded configuration once the sections the caller depends on
// have been checked.
//
// This is the only way a command reaches the configuration, deliberately: there is no
// accessor that hands back the file unchecked, because the check is what decides whether
// the fields a command is about to read mean anything.
//
// A command names what it reads and nothing more, so a missing field is reported against
// the command that needs it and one mechanism's configuration never blocks the other's.
// The check happens before any AWS call, and reports every problem it finds at once, so a
// misconfigured deployment is described in full rather than one field per run.
func requireConfig(sections ...config.Section) (*config.Config, error) {
	if appCfg == nil {
		return nil, fmt.Errorf("configuration not loaded")
	}
	if err := appCfg.Require(sections...); err != nil {
		return nil, err
	}
	return appCfg, nil
}

// warnf reports a non-fatal problem on the command's error stream. It is for work that
// failed after the primary mutation already succeeded, where the command must still
// report success — writing through the command keeps the output capturable in tests.
func warnf(cmd *cobra.Command, format string, args ...any) {
	fmt.Fprintf(cmd.ErrOrStderr(), "Warning: "+format+"\n", args...)
}
