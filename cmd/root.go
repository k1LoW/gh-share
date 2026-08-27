package cmd

import (
	"fmt"
	"os"
)

var rootCmd = newShareCommand()

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
