package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"libs.altipla.consulting/collections"
	"libs.altipla.consulting/errors"
	"libs.altipla.consulting/watch"
)

var (
	flagWatch       []string
	flagIgnore      []string
	flagRestart     bool
	flagRestartExts []string
)

func init() {
	CmdRoot.AddCommand(CmdRun)
	CmdRun.PersistentFlags().StringSliceVarP(&flagWatch, "watch", "w", nil, "Folders to watch recursively for changes")
	CmdRun.PersistentFlags().StringSliceVarP(&flagIgnore, "ignore", "g", nil, "Folders to ignore")
	CmdRun.PersistentFlags().BoolVarP(&flagRestart, "restart", "r", false, "Automatic restart in case of failure")
	CmdRun.PersistentFlags().StringSliceVarP(&flagRestartExts, "restart-exts", "e", nil, "List of extensions that cause the app to restart")
}

var CmdRun = &cobra.Command{
	Use:   "run",
	Short: "Run a command everytime the package changes.",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(command *cobra.Command, args []string) error {
		changes := make(chan string)
		rebuild := make(chan bool, 1)
		rerun := make(chan bool, 1)

		// First rebuild of the app after a change
		rebuild <- true

		ctx, cancel := context.WithCancel(context.Background())
		g, ctx := errgroup.WithContext(ctx)

		for _, folder := range flagWatch {
			watchFolder(ctx, g, changes, folder)
		}
		watchFolder(ctx, g, changes, args[0])

		g.Go(func() error {
			var pendingBuild bool
			var waitNextChange *time.Timer
			var ch <-chan time.Time

			for {
				if waitNextChange != nil {
					ch = waitNextChange.C
				}

				select {
				case <-ctx.Done():
					return nil

				case change := <-changes:
					if filepath.Ext(change) == ".go" {
						log.WithField("path", change).Debug("File change detected, rebuild")

						pendingBuild = true
					} else if collections.HasString(flagRestartExts, filepath.Ext(change)) {
						log.WithField("path", change).Debug("File change detected, restart")
					} else {
						log.WithField("path", change).Debug("File change detected, but no action performed")
						continue
					}

					if waitNextChange == nil {
						waitNextChange = time.NewTimer(50 * time.Millisecond)
					} else {
						if !waitNextChange.Stop() {
							<-waitNextChange.C
						}
						waitNextChange.Reset(50 * time.Millisecond)
					}

				case <-ch:
					waitNextChange = nil

					if pendingBuild {
						select {
						case rebuild <- true:
						default:
						}
						pendingBuild = false

						break
					}

					select {
					case rerun <- true:
					default:
					}
				}
			}
		})

		g.Go(func() error {
			for {
				select {
				case <-ctx.Done():
					return nil

				case <-rebuild:
					log.Info(">>> build...")

					cmd := exec.CommandContext(ctx, "go", "install", args[0])
					cmd.Stdin = os.Stdin
					cmd.Stdout = os.Stdout
					cmd.Stderr = os.Stderr
					if err := cmd.Run(); err != nil {
						if _, ok := err.(*exec.ExitError); ok {
							log.Error(">>> command failed!")
							continue
						}

						return errors.Trace(err)
					}

					select {
					case rerun <- true:
					default:
					}
				}
			}
		})

		g.Go(func() error {
			r := new(runner)
			var close chan error
			secs := 1 * time.Second
			for {
				select {
				case <-ctx.Done():
					return nil

				case <-rerun:
					if err := r.Stop(); err != nil {
						return errors.Trace(err)
					}

					log.Info(">>> run...")

					name := filepath.Base(args[0])
					if args[0] == "." {
						wd, err := os.Getwd()
						if err != nil {
							return errors.Trace(err)
						}
						name = filepath.Base(wd)
					}
					close = r.Start(ctx, name, args[1:]...)

				case appErr := <-close:
					if flagRestart {
						if appErr != nil {
							log.WithField("error", appErr.Error()).Errorf(">>> command failed, restarting in %s", secs)
						} else {
							log.Errorf(">>> command exited, restarting in %s", secs)
						}

						// Wait for sometime before restarting the process
						select {
						case <-ctx.Done():
							return nil
						case <-time.After(secs):
						}
						secs = secs * 2
						if secs > 8*time.Second {
							secs = 8 * time.Second
						}

						// Force rerun
						select {
						case rerun <- true:
						default:
						}
					} else {
						if appErr != nil {
							log.WithField("error", appErr.Error()).Errorf(">>> command failed")
						}
					}
				}
			}
		})

		g.Go(func() error {
			watch.Interrupt(ctx, cancel)
			return nil
		})

		return errors.Trace(g.Wait())
	},
}

type runner struct {
	cmd *exec.Cmd
}

func (r *runner) Start(ctx context.Context, name string, args ...string) chan error {
	r.cmd = exec.CommandContext(ctx, name, args...)
	r.cmd.Stdin = os.Stdin
	r.cmd.Stdout = os.Stdout
	r.cmd.Stderr = os.Stderr

	close := make(chan error, 1)
	go func() {
		close <- errors.Trace(r.cmd.Run())
	}()
	return close
}

func (r *runner) Stop() error {
	if r.cmd != nil && (r.cmd.ProcessState == nil || !r.cmd.ProcessState.Exited()) {
		if err := r.cmd.Process.Signal(os.Interrupt); err != nil {
			return errors.Trace(err)
		}
		if err := r.cmd.Wait(); err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

func watchFolder(ctx context.Context, g *errgroup.Group, changes chan string, folder string) {
	g.Go(func() error {
		var paths []string
		walkFn := func(path string, info os.FileInfo, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return errors.Trace(err)
			}
			if !info.IsDir() {
				return nil
			}

			var ignore bool
			for _, ig := range flagIgnore {
				if strings.HasPrefix(path, ig) {
					ignore = true
					break
				}
			}
			if ignore || path == "node_modules" || path == ".git" {
				return filepath.SkipDir
			}

			paths = append(paths, path)

			return nil
		}
		if err := filepath.Walk(folder, walkFn); err != nil {
			return errors.Trace(err)
		}

		log.WithField("path", folder).Debug("Watching changes")
		return errors.Trace(watch.Files(ctx, changes, paths...))
	})
}
