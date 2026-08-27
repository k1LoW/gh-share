package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{Use: "gh-share", Short: "Upload files to a repository-scoped GitHub Actions artifact"}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() { rootCmd.AddCommand(newShareCommand()) }
