// Command casebox is the Casebox CLI and worker.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/alternayte/casebox/cli/internal/buildinfo"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "casebox",
		Short:         "Stop steering your coding agents.",
		Version:       buildinfo.Version,
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.SetVersionTemplate(fmt.Sprintf("casebox %s\n", buildinfo.Version))
	root.AddCommand(newUpCommand(), newDownCommand(), newPauseCommand(), newResumeCommand(), newLinkCommand(),
		newImportCommand(), newHookCommand(), newCaptureCommand(), newInitCommand(), newJoinCommand(), newDoctorCommand(), newEraseCommand(), newWorkerCommand(), newEnvCommand(), newSteeringCommand(),
		newMineCommand(), newReviewCommand(), newCasesCommand())
	return root
}
