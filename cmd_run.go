package main

import (
	"context"
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/altipla-consulting/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/exp/slices"
	"golang.org/x/sync/errgroup"
	"libs.altipla.consulting/watch"
)

var defaultIgnoreFolders = []string{
	"node_modules",
	".git",
}

type empty struct{}

var cmdRun = &cobra.Command{
	Use:     "run",
	Example: "reloader run -r ./backend",
	Short:   "Run a command everytime the package changes.",
	Args:    cobra.MinimumNArgs(1),
}

func init() {
	var flagWatch, flagIgnore []string
	var flagRestartExts []string
	var flagRestart bool
	cmdRun.PersistentFlags().StringSliceVarP(&flagWatch, "watch", "w", nil, "Folders to watch recursively for changes.")
	cmdRun.PersistentFlags().StringSliceVarP(&flagIgnore, "ignore", "g", nil, "Folders to ignore.")
	cmdRun.PersistentFlags().BoolVarP(&flagRestart, "restart", "r", false, "Automatic restart in case of failure.")
	cmdRun.PersistentFlags().StringSliceVarP(&flagRestartExts, "restart-exts", "e", nil, "List of extensions that cause the app to restart.")

	cmdRun.RunE = func(cmd *cobra.Command, args []string) error {
		grp, ctx := errgroup.WithContext(cmd.Context())

		changes := make(chan string)
		for _, folder := range flagWatch {
			grp.Go(watchFolder(ctx, changes, flagIgnore, folder))
		}
		grp.Go(watchFolder(ctx, changes, flagIgnore, args[0]))

		rebuild := make(chan empty)
		restart := make(chan empty, 1)
		grp.Go(receiveWatchChanges(ctx, changes, flagRestartExts, rebuild, restart))

		grp.Go(appManager(ctx, args, flagRestart, rebuild, restart))

		return errors.Trace(grp.Wait())
	}
}

func watchFolder(ctx context.Context, changes chan string, ignore []string, folder string) func() error {
	return func() error {
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

			// Ignore default and custom folders.
			if slices.Contains(defaultIgnoreFolders, filepath.Base(path)) {
				return filepath.SkipDir
			}
			for _, ig := range ignore {
				if strings.HasPrefix(path, ig) {
					return filepath.SkipDir
				}
			}

			paths = append(paths, path)

			return nil
		}
		if err := filepath.Walk(folder, walkFn); err != nil {
			return errors.Trace(err)
		}

		log.WithField("path", folder).Debug("Watching changes")
		return errors.Trace(watch.Files(ctx, changes, paths...))
	}
}

func receiveWatchChanges(ctx context.Context, changes chan string, restartExts []string, rebuild, restart chan empty) func() error {
	return func() error {
		// Batch changes with a short timer to avoid concurrency issues with atomic saving.
		// Also depending on the changed file we need a build or only to restart the app.
		var buildPending bool
		var waitNextChange *time.Timer

		for {
			var ch <-chan time.Time
			if waitNextChange != nil {
				ch = waitNextChange.C
			}

			select {
			case <-ctx.Done():
				return nil

			case change := <-changes:
				if filepath.Ext(change) == ".go" {
					log.WithField("path", change).Debug("File change detected, rebuild")
					buildPending = true
				} else if slices.Contains(restartExts, filepath.Ext(change)) {
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

				if buildPending {
					select {
					case rebuild <- empty{}:
					default:
					}
					buildPending = false
				} else {
					select {
					case restart <- empty{}:
					default:
					}
				}
			}
		}
	}
}

var errBuildFailed = errors.New("reloader: build failed")

func buildApp(ctx context.Context, app string, restart chan empty) error {
	log.Info(">>> build...")

	cmd := exec.CommandContext(ctx, "go", "install", app)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			log.Error(">>> build command failed!")
			return errors.Trace(errBuildFailed)
		}

		return errors.Trace(err)
	}

	select {
	case restart <- empty{}:
	default:
	}

	return nil
}

func appManager(ctx context.Context, args []string, shouldRestart bool, rebuild, restart chan empty) func() error {
	return func() error {
		// Build the application for the first time when starting up.
		if err := buildApp(ctx, args[0], restart); err != nil && !errors.Is(err, errBuildFailed) {
			return errors.Trace(err)
		}

		var cmd *exec.Cmd
		runerr := make(chan error, 1)
		secs := 1 * time.Second

		for {
			select {
			case <-ctx.Done():
				return nil

			case <-rebuild:
				if err := stopProcess(ctx, cmd, runerr); err != nil {
					return errors.Trace(err)
				}
				cmd = nil

				if err := buildApp(ctx, args[0], restart); err != nil {
					if errors.Is(err, errBuildFailed) {
						continue
					}

					return errors.Trace(err)
				}

				// Reset the restart timer after a successful build.
				secs = 1 * time.Second

				select {
				case restart <- empty{}:
				default:
				}

			case <-restart:
				if err := stopProcess(ctx, cmd, runerr); err != nil {
					return errors.Trace(err)
				}

				log.Info(">>> run...")
				var err error
				cmd, err = startProcess(ctx, runerr, args)
				if err != nil {
					return errors.Trace(err)
				}

			case appErr := <-runerr:
				cmd = nil

				if shouldRestart {
					if appErr != nil {
						log.WithField("error", appErr.Error()).Errorf(">>> command failed, restarting in %s", secs)
					} else {
						log.Errorf(">>> command exited, restarting in %s", secs)
					}

					// Wait a little bit before restarting the failing process.
					select {
					case <-ctx.Done():
						return nil
					case <-time.After(secs):
					}
					secs = secs * 2
					if secs > 8*time.Second {
						secs = 8 * time.Second
					}

					// Run application again.
					restart <- empty{}
				} else {
					if appErr != nil {
						log.WithField("error", appErr.Error()).Errorf(">>> command failed")
					}
				}
			}
		}
	}
}

func startProcess(ctx context.Context, runerr chan error, args []string) (*exec.Cmd, error) {
	name := filepath.Base(args[0])
	if args[0] == "." {
		wd, err := os.Getwd()
		if err != nil {
			return nil, errors.Trace(err)
		}
		name = filepath.Base(wd)
	}
	cmd := exec.CommandContext(ctx, filepath.Join(build.Default.GOPATH, "bin", name), args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, errors.Trace(err)
	}

	go func() {
		runerr <- errors.Trace(cmd.Wait())
	}()

	return cmd, nil
}

func stopProcess(ctx context.Context, cmd *exec.Cmd, runerr chan error) error {
	if cmd == nil {
		return nil
	}

	logger := log.WithField("pid", cmd.Process.Pid)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	grp, ctx := errgroup.WithContext(ctx)

	grp.Go(func() error {
		logger.Trace("Send interrupt signal")
		return errors.Trace(cmd.Process.Signal(os.Interrupt))
	})

	grp.Go(func() error {
		appErr := <-runerr
		logger.WithField("error", appErr).Trace("Process close detected")
		cancel()
		return nil
	})

	grp.Go(func() error {
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
			log.Info(">>> close process...")
		}
		return nil
	})

	grp.Go(func() error {
		select {
		case <-ctx.Done():
			logger.Trace("Process closed before the timeout")
			return nil
		case <-time.After(15 * time.Second):
			logger.Warning("Kill process after timeout")
			return errors.Trace(cmd.Process.Kill())
		}
	})

	if err := grp.Wait(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return errors.Trace(err)
	}

	return nil
}
