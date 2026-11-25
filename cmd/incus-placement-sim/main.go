package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	scriptletLoad "github.com/lxc/incus/v7/internal/server/scriptlet/load"
	"github.com/lxc/incus/v7/internal/version"
)

type cmdGlobal struct {
	flagHelp    bool
	flagVersion bool
}

func main() {
	app := &cobra.Command{}
	app.Use = "incus-placement-sim"
	app.Short = "Develop and test instance placement scriptlets"
	app.Long = `Description:
  Develop and test instance placement scriptlets

  This tool simulates how an instance placement scriptlet responds to
  instance creation, moves and cluster member evacuation, using a
  description of a cluster captured from a real deployment.
`
	app.SilenceUsage = true
	app.CompletionOptions = cobra.CompletionOptions{DisableDefaultCmd: true}

	// Global flags.
	globalCmd := cmdGlobal{}
	app.PersistentFlags().BoolVar(&globalCmd.flagVersion, "version", false, "Print version number")
	app.PersistentFlags().BoolVarP(&globalCmd.flagHelp, "help", "h", false, "Print help")

	// Help handling.
	app.SetHelpCommand(&cobra.Command{
		Use:    "no-help",
		Hidden: true,
	})

	// Version handling.
	app.SetVersionTemplate("{{.Version}}\n")
	app.Version = version.Version

	// validate sub-command.
	validateCmd := cmdValidate{global: &globalCmd}
	app.AddCommand(validateCmd.command())

	// Run the main command and handle errors.
	err := app.Execute()
	if err != nil {
		os.Exit(1)
	}
}

type cmdValidate struct {
	global *cmdGlobal
}

func (c *cmdValidate) command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = "validate <scriptlet>"
	cmd.Short = "Validate an instance placement scriptlet"
	cmd.Long = `Description:
  Validate an instance placement scriptlet

  This performs the same checks as the server does when the
  instances.placement.scriptlet configuration key is set.
`
	cmd.Args = cobra.ExactArgs(1)
	cmd.RunE = c.run

	return cmd
}

func (c *cmdValidate) run(cmd *cobra.Command, args []string) error {
	src, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}

	err = scriptletLoad.InstancePlacementValidate(string(src))
	if err != nil {
		return err
	}

	fmt.Println("Scriptlet is valid")

	return nil
}
