package main

import (
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var flagDebug bool

func init() {
	CmdRoot.PersistentFlags().BoolVarP(&flagDebug, "debug", "d", false, "Enable debug logging for this tool.")
}

var CmdRoot = &cobra.Command{
	Use:   "reloader",
	Short: "Build & run a Go app or its tests for every change.",
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		log.SetFormatter(&log.TextFormatter{
			ForceColors: true,
		})
		if flagDebug {
			log.SetLevel(log.DebugLevel)
		}
	},
}
