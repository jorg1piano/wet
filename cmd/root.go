package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "wet",
	Short: "Developer flow cli",
	Long:  "A CLI tool for managing git worktrees and their environments.",

	// Execute prints the error itself; SilenceErrors keeps cobra from
	// printing it a second time.
	SilenceErrors: true,

	// Suppress the usage dump for runtime failures (a hook exiting nonzero, a
	// missing template key) while keeping it for genuine misuse. Cobra runs
	// this hook after argument validation and before RunE, which is exactly
	// the line between the two: anything that fails later is the command
	// working as invoked and failing at its job.
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		cmd.SilenceUsage = true
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
