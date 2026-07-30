package cmd

import "github.com/spf13/cobra"

var openSandboxComponentName string

var openSandboxCmd = &cobra.Command{
	Use:   "opensandbox",
	Short: "Use an injected OpenSandbox Execd component",
	Long:  "Resolve and authenticate a Fast Sandbox route, then delegate command and file operations to the official OpenSandbox Go SDK.",
}

func init() {
	rootCmd.AddCommand(openSandboxCmd)
	openSandboxCmd.PersistentFlags().StringVar(
		&openSandboxComponentName,
		"component",
		"execd",
		"Pool Infra Component name that implements the OpenSandbox Execd protocol",
	)
}
