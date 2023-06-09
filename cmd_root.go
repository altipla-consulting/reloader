package main

import (
	"github.com/altipla-consulting/cmdbase"
	"github.com/spf13/cobra"
)

func main() {
	cmdbase.Main()
}

var flagDebug bool
var cmdRoot *cobra.Command

func init() {
	cmdRoot = cmdbase.CmdRoot(
		"reloader",
		"Build & run a Go app or its tests for every change.",
		cmdbase.WithUpdate("github.com/altipla-consulting/reloader"),
		cmdbase.WithInstall())
	cmdRoot.AddCommand(cmdRun)
	cmdRoot.AddCommand(cmdTest)
}
