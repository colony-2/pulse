package pulsecli

import (
	providercli "github.com/colony-2/pulse-macvz/cli"
	"github.com/spf13/cobra"
)

func newVZCommand() *cobra.Command {
	vzCmd := &cobra.Command{Use: "vz", Short: "Manage local VZ capacity"}
	setup, upgrade := providercli.NewSetupCommand(), providercli.NewUpgradeCommand()
	for _, cmd := range []*cobra.Command{setup, upgrade} {
		setVZCommandDefault(cmd, "macos-docker-capacity", "0")
	}
	vzCmd.AddCommand(setup, upgrade)
	return vzCmd
}

func setVZCommandDefault(cmd *cobra.Command, name, value string) {
	// Set the default without marking the flag explicit so saved settings apply.
	flag := cmd.Flags().Lookup(name)
	if err := flag.Value.Set(value); err != nil {
		panic(err)
	}
	flag.DefValue = value
}
