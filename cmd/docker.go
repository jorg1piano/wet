package cmd

import "github.com/spf13/cobra"

var dockerCmd = &cobra.Command{
	Use:   "docker",
	Short: "Manage Docker resources owned by wet",
}

func init() {
	rootCmd.AddCommand(dockerCmd)
}
