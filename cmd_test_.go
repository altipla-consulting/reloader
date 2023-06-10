package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/altipla-consulting/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"libs.altipla.consulting/watch"
)

var cmdTest = &cobra.Command{
	Use:     "test",
	Example: "reloader test ./my/package",
	Short:   "Run Go tests everytime the package changes.",
	Args:    cobra.MinimumNArgs(1),
}

func init() {
	var flagVerbose bool
	var flagRun, flagTags string
	var flagCount int64
	cmdTest.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "Verbose run of the go tests.")
	cmdTest.PersistentFlags().StringVarP(&flagRun, "run", "r", "", "Run only those tests and examples matching the regular expression.")
	cmdTest.PersistentFlags().StringVarP(&flagTags, "tags", "t", "", "Tags for the go build command.")
	cmdTest.PersistentFlags().Int64VarP(&flagCount, "count", "c", 0, "Run tests multiple times. If count is 0 it will run one time. If count is 1 it will run one time but without caching the result (standard go test behavior).")

	cmdTest.RunE = func(cmd *cobra.Command, args []string) error {
		changes := make(chan string)
		reload := make(chan bool, 1)

		g, ctx := errgroup.WithContext(cmd.Context())

		g.Go(func() error {
			return errors.Trace(watch.Recursive(ctx, changes, args...))
		})

		g.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return nil

				case change := <-changes:
					log.WithField("path", change).Debug("File change detected")

					select {
					case reload <- true:
					default:
					}
				}
			}
		})

		g.Go(func() error {
			// First test run.
			reload <- true

			for {
				select {
				case <-ctx.Done():
					return nil

				case <-reload:
					log.Info(">>> test...")

					runCmd := []string{"test"}
					if flagVerbose {
						runCmd = append(runCmd, "-v")
					}
					if flagRun != "" {
						runCmd = append(runCmd, "-run", flagRun)
					}
					if flagTags != "" {
						runCmd = append(runCmd, "-tags", flagTags)
					}
					if flagCount > 0 {
						runCmd = append(runCmd, "-count", fmt.Sprint(flagCount))
					}
					runCmd = append(runCmd, args...)
					cmd := exec.CommandContext(ctx, "go", runCmd...)
					cmd.Stdin = os.Stdin
					cmd.Stdout = os.Stdout
					cmd.Stderr = os.Stderr
					if err := cmd.Run(); err != nil {
						if ctx.Err() != nil {
							return nil
						}

						if _, ok := err.(*exec.ExitError); ok {
							log.Error(">>> command failed!")
							continue
						}

						return errors.Trace(err)
					}

					log.Info(">>> waiting...")
				}
			}
		})

		if err := g.Wait(); err != nil {
			log.Println("wait err", err)
			return errors.Trace(err)
		}

		return nil
	}
}
