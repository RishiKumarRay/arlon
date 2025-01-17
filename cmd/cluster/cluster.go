package cluster

import "github.com/spf13/cobra"

func NewCommand() *cobra.Command {
	command := &cobra.Command{
		Use:               "cluster",
		Short:             "Manage clusters",
		Long:              "Manage clusters",
		DisableAutoGenTag: true,
		Run: func(c *cobra.Command, args []string) {
		},
	}
	command.AddCommand(deployClusterCommand())
	return command
}

