package main

import (
	"context"
	"go/build"
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
	CmdRun.PersistentFlags().StringSliceVarP(&flagWatch, "watch", "w", nil, "Folders to watch recursively for changes.")
	CmdRun.PersistentFlags().StringSliceVarP(&flagIgnore, "ignore", "g", nil, "Folders to ignore.")
	CmdRun.PersistentFlags().BoolVarP(&flagRestart, "restart", "r", false, "Automatic restart in case of failure.")
	CmdRun.PersistentFlags().StringSliceVarP(&flagRestartExts, "restart-exts", "e", nil, "List of extensions that cause the app to restart.")
}

type empty struct{}

type actionsController struct {
	changes chan string
	rebuild chan empty
	restart chan empty
	runerr  chan error
}

var CmdRun = &cobra.Command{
	Use:   "run",
	Short: "Run a command everytime the package changes.",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(command *cobra.Command, args []string) error {
		ctx, cancel := context.WithCancel(context.Background())
		g, ctx := errgroup.WithContext(ctx)

		actions := &actionsController{
			changes: make(chan string),

			// Multiple messages have the same effect that a single one, so buffer the channels.
			rebuild: make(chan empty, 1),
			restart: make(chan empty, 1),
			runerr:  make(chan error),
		}

		// Watch the folders for changes.
		for _, folder := range flagWatch {
			g.Go(watchFolder(ctx, actions, folder))
		}
		g.Go(watchFolder(ctx, actions, args[0]))
		g.Go(receiveWatchChanges(ctx, actions))

		// Managers of the rebuild and rerun process.
		g.Go(buildsManager(ctx, actions, args[0]))
		g.Go(restartsManager(ctx, actions))
		g.Go(appManager(ctx, actions, args))

		// Watch for close interrupts to exit.
		g.Go(func() error {
			watch.Interrupt(ctx, cancel)
			return nil
		})

		return errors.Trace(g.Wait())
	},
}

func watchFolder(ctx context.Context, actions *actionsController, folder string) func() error {
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
		return errors.Trace(watch.Files(ctx, actions.changes, paths...))
	}
}

func receiveWatchChanges(ctx context.Context, actions *actionsController) func() error {
	return func() error {
		// Batch changes with a short timer to avoid concurrency issues with atomic saving.
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

			case change := <-actions.changes:
				if filepath.Ext(change) == ".go" {
					log.WithField("path", change).Debug("File change detected, rebuild")
					buildPending = true
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

				if buildPending {
					select {
					case actions.rebuild <- empty{}:
					default:
					}
					buildPending = false
				} else {
					select {
					case actions.restart <- empty{}:
					default:
					}
				}
			}
		}
	}
}

func buildApp(ctx context.Context, app string) error {
	cmd := exec.CommandContext(ctx, "go", "install", app)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			log.Error(">>> command failed!")
			return nil
		}

		return errors.Trace(err)
	}

	return nil
}

func buildsManager(ctx context.Context, actions *actionsController, app string) func() error {
	return func() error {
		// Build the application for the first time when starting up.
		if err := buildApp(ctx, app); err != nil {
			return errors.Trace(err)
		}
		select {
		case actions.restart <- empty{}:
		default:
		}

		for {
			select {
			case <-ctx.Done():
				return nil

			case <-actions.rebuild:
				log.Info(">>> build...")

				if err := buildApp(ctx, app); err != nil {
					return errors.Trace(err)
				}
				select {
				case actions.restart <- empty{}:
				default:
				}
			}
		}
	}
}

func restartsManager(ctx context.Context, actions *actionsController) func() error {
	return func() error {
		secs := 1 * time.Second

		for {
			select {
			case <-ctx.Done():
				return nil

			case appErr := <-actions.runerr:
				if flagRestart {
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
					actions.restart <- empty{}
				} else {
					if appErr != nil {
						log.WithField("error", appErr.Error()).Errorf(">>> command failed")
					}
				}
			}
		}
	}
}

func appManager(ctx context.Context, actions *actionsController, args []string) func() error {
	return func() error {
		var cmd *exec.Cmd
		var monitor chan error

		for {
			select {
			case <-ctx.Done():
				return nil

			case <-actions.restart:
				if cmd != nil && (cmd.ProcessState == nil || !cmd.ProcessState.Exited()) {
					if err := cmd.Process.Signal(os.Interrupt); err != nil {
						return errors.Trace(err)
					}
				}
				monitor = nil

				log.Info(">>> run...")
				name := filepath.Base(args[0])
				if args[0] == "." {
					wd, err := os.Getwd()
					if err != nil {
						return errors.Trace(err)
					}
					name = filepath.Base(wd)
				}
				cmd = exec.CommandContext(ctx, filepath.Join(build.Default.GOPATH, "bin", name), args[1:]...)
				cmd.Stdin = os.Stdin
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr

				monitor = make(chan error, 1)
				go watchProcess(monitor, cmd)()

			case appErr := <-monitor:
				log.WithField("error", appErr.Error()).Debug("Error received from monitor")
				actions.runerr <- errors.Trace(appErr)
			}
		}
	}
}

func watchProcess(monitor chan error, cmd *exec.Cmd) func() {
	return func() {
		monitor <- errors.Trace(cmd.Run())
	}
}
