package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/umccr/data-releaser/internal/config"
)

// shareRecordsTable returns the DynamoDB table name for Share_Records, used by all
// subcommands that interact with the authorization store.
//
// The Infrastructure_Stack creates the table as "<namePrefix>-ShareRecords", so this
// cannot be a bare constant: a stack deployed under a non-default name prefix would
// not be found. It comes from config, falling back to the name the stack produces
// under its default prefix.
func shareRecordsTable() string {
	if cfg := GetConfig(); cfg != nil && cfg.Authorization.ShareRecordsTable != "" {
		return cfg.Authorization.ShareRecordsTable
	}
	return config.DefaultShareRecordsTable
}

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
