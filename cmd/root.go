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
	Short: "Grant external researchers time-bounded access to bioinformatics libraries via htsget-rs",
	Long: `Data Releaser is a CLI tool that enables UMCCR administrators to share
bioinformatics data (identified by library_id) with external researchers
via the htsget-rs protocol. It manages authorization, VPC Lattice connectivity,
and manifest delivery across multiple AWS accounts.`,
	// Execute renders the error itself, once. Without these, cobra also prints it and
	// appends a full usage dump, so an operational failure (bad credentials, missing
	// table) was reported two or three times and buried under flag documentation.
	SilenceErrors: true,
	SilenceUsage:  true,
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

// GetConfig returns the loaded application configuration.
// Must be called after PersistentPreRunE has executed.
func GetConfig() *config.Config {
	return appCfg
}

// warnf reports a non-fatal problem on the command's error stream. It is for work that
// failed after the primary mutation already succeeded, where the command must still
// report success — writing through the command keeps the output capturable in tests.
func warnf(cmd *cobra.Command, format string, args ...any) {
	fmt.Fprintf(cmd.ErrOrStderr(), "Warning: "+format+"\n", args...)
}
