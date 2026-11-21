package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/ChurroLoco/go-tor-cli/internal/app"
	"github.com/ChurroLoco/go-tor-cli/internal/config"
	"github.com/ChurroLoco/go-tor-cli/internal/daemon"
	"github.com/ChurroLoco/go-tor-cli/internal/tui"
	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
)

type connectFunc func(context.Context, config.Config) (app.Service, func(), error)

func NewRoot() (*cobra.Command, error) {
	cfg, err := config.Default()
	if err != nil {
		return nil, err
	}
	return newRoot(cfg, func(ctx context.Context, cfg config.Config) (app.Service, func(), error) {
		c, err := daemon.Connect(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		return c, c.Close, nil
	}, tui.Run), nil
}

func newRoot(cfg config.Config, connect connectFunc, runUI func(context.Context, app.Service) error) *cobra.Command {
	var jsonOutput bool
	root := &cobra.Command{
		Use:           "gotorrent",
		Short:         "gotor-cli: a scriptable BitTorrent client and terminal UI",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&cfg.StateDir, "state-dir", cfg.StateDir, "Daemon socket, state, and logs directory")
	root.PersistentFlags().StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "Download directory (used when starting the daemon)")
	root.PersistentFlags().BoolVar(&jsonOutput, "json", false, "Print machine-readable JSON")
	withService := func(cmd *cobra.Command, fn func(app.Service) error) error {
		resolved, err := cfg.Absolute()
		if err != nil {
			return err
		}
		svc, close, err := connect(cmd.Context(), resolved)
		if err != nil {
			return err
		}
		defer close()
		return fn(svc)
	}
	show := func(cmd *cobra.Command, value any) error {
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
		}
		switch v := value.(type) {
		case app.Torrent:
			_, err := fmt.Fprintf(cmd.OutOrStdout(),
				"ID: %s\nName: %s\nStatus: %s\nProgress: %.1f%%\nSize: %s\nDownloaded: %s\nUploaded: %s\nRatio: %.2f\nDownload: %s/s\nUpload: %s/s\nPeers: %d\nETA: %s\n",
				v.ID, safeText(v.Name), v.Status, v.Progress*100, size(v.Size), size(v.Downloaded), size(v.Uploaded), v.Ratio, size(int64(v.DownloadRate)), size(int64(v.UploadRate)), v.Peers, eta(v.ETASeconds))
			if err != nil {
				return err
			}
			for _, file := range v.Files {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s / %s\n", safeText(file.Path), size(file.Downloaded), size(file.Size)); err != nil {
					return err
				}
			}
			return nil
		case []app.Torrent:
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			if _, err := fmt.Fprintln(w, "ID\tNAME\tSTATUS\tPROGRESS\tSIZE\tDOWN/s\tUP/s\tPEERS\tETA"); err != nil {
				return err
			}
			for _, t := range v {
				if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%.1f%%\t%s\t%s\t%s\t%d\t%s\n",
					t.ID, safeText(t.Name), t.Status, t.Progress*100, size(t.Size),
					size(int64(t.DownloadRate)), size(int64(t.UploadRate)), t.Peers, eta(t.ETASeconds)); err != nil {
					return err
				}
			}
			return w.Flush()
		default:
			_, err := fmt.Fprintln(cmd.OutOrStdout(), value)
			return err
		}
	}
	ui := func(cmd *cobra.Command, _ []string) error {
		if jsonOutput {
			return fmt.Errorf("--json requires a non-interactive command")
		}
		return withService(cmd, func(svc app.Service) error { return runUI(cmd.Context(), svc) })
	}
	root.Args = cobra.NoArgs
	root.RunE = ui
	root.AddCommand(&cobra.Command{Use: "ui", Short: "Open the interactive terminal UI", Args: cobra.NoArgs, RunE: ui})
	root.AddCommand(&cobra.Command{
		Use: "add <magnet-or-torrent>", Short: "Add a magnet URI or local .torrent file", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withService(cmd, func(svc app.Service) error {
				var t app.Torrent
				var err error
				if strings.HasPrefix(strings.ToLower(args[0]), "magnet:") {
					t, err = svc.AddMagnet(cmd.Context(), args[0])
				} else {
					t, err = svc.AddTorrentFile(cmd.Context(), args[0])
				}
				if err != nil {
					return fmt.Errorf("add torrent: %w", err)
				}
				return show(cmd, t)
			})
		},
	})
	root.AddCommand(&cobra.Command{
		Use: "list", Short: "List torrents", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withService(cmd, func(svc app.Service) error {
				items, err := svc.Torrents(cmd.Context())
				if err != nil {
					return fmt.Errorf("list torrents: %w", err)
				}
				if items == nil {
					items = []app.Torrent{}
				}
				return show(cmd, items)
			})
		},
	})
	root.AddCommand(&cobra.Command{
		Use: "status <id>", Short: "Show torrent statistics and files", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withService(cmd, func(svc app.Service) error {
				t, err := svc.Torrent(cmd.Context(), args[0])
				if err != nil {
					return fmt.Errorf("get torrent status: %w", err)
				}
				return show(cmd, t)
			})
		},
	})
	for _, operation := range []string{"pause", "resume", "remove"} {
		var deleteData bool
		cmd := &cobra.Command{
			Use: operation + " <id>", Short: strings.ToUpper(operation[:1]) + operation[1:] + " a torrent", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return withService(cmd, func(svc app.Service) error {
					var err error
					switch operation {
					case "pause":
						err = svc.Pause(cmd.Context(), args[0])
					case "resume":
						err = svc.Resume(cmd.Context(), args[0])
					case "remove":
						err = svc.Remove(cmd.Context(), args[0], deleteData)
					}
					if err != nil {
						return fmt.Errorf("%s torrent: %w", operation, err)
					}
					if jsonOutput {
						return show(cmd, map[string]any{"id": args[0], "operation": operation, "ok": true})
					}
					return show(cmd, operation+": "+args[0])
				})
			},
		}
		if operation == "remove" {
			cmd.Flags().BoolVar(&deleteData, "delete-data", false, "Also delete this torrent's downloaded files")
		}
		root.AddCommand(cmd)
	}
	root.AddCommand(&cobra.Command{
		Use: "serve", Short: "Run the torrent daemon in the foreground (Ctrl+C stops it)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := cfg.Absolute()
			if err != nil {
				return err
			}
			logger := slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), nil))
			return daemon.Serve(cmd.Context(), resolved, logger)
		},
	})
	root.AddCommand(&cobra.Command{
		Use: "shutdown", Short: "Stop the daemon cleanly without removing torrents", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved, err := cfg.Absolute()
			if err != nil {
				return err
			}
			client := daemon.NewClient(resolved.StateDir)
			defer client.Close()
			if err := client.Shutdown(cmd.Context()); err != nil {
				return fmt.Errorf("shutdown daemon: %w", err)
			}
			if jsonOutput {
				return show(cmd, map[string]bool{"ok": true})
			}
			return show(cmd, "Daemon stopped.")
		},
	})
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	return root
}

func size(n int64) string {
	if n < 0 {
		n = 0
	}
	return humanize.Bytes(uint64(n))
}

func eta(seconds int64) string {
	if seconds < 0 {
		return "unknown"
	}
	return fmt.Sprintf("%ds", seconds)
}

func safeText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}
