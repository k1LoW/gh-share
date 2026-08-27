package cmd

import (
	"fmt"
	"os"

	"github.com/k1LoW/gh-share/version"
	"github.com/spf13/cobra"
)

var rootCmd = func() *cobra.Command {
	cmd := newShareCommand()
	cmd.Version = version.Version
	return cmd
}()

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
