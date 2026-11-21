# gotor-cli

A Go BitTorrent client with a scriptable Cobra CLI and a full-screen Bubble Tea
v2 terminal interface. The executable is named `gotorrent`.

## Build

Requires **Go 1.26 or newer**, Linux or macOS (local IPC uses Unix sockets), and
an interactive terminal for the TUI.

```sh
go build -o gotorrent ./cmd/gotorrent
go fmt ./...
go vet ./...
go test ./...
```

## Quick start

```sh
./gotorrent add /absolute/path/to/example.torrent
./gotorrent add 'magnet:?xt=urn:btih:YOUR_INFO_HASH'
./gotorrent                    # no subcommand opens the TUI
./gotorrent ui                 # same as above
./gotorrent list
./gotorrent list --json
./gotorrent status TORRENT_ID --json
./gotorrent pause TORRENT_ID
./gotorrent resume TORRENT_ID
./gotorrent remove TORRENT_ID   # keeps downloaded files
./gotorrent remove TORRENT_ID --delete-data
./gotorrent shutdown
```

Use the ID printed by `add` or `list`. Quote magnet links so the shell does not
interpret their `&` characters. Metadata and piece verification are asynchronous:
an added magnet can initially show an unknown name, size, or ETA. Downloads need
reachable peers; an unseeded torrent will not make progress.

### Daemon and storage

The first CLI command or TUI session automatically starts a local background
daemon. **Downloads continue when a command exits or the TUI closes.** All
commands using the same state directory control the same engine, including while
the TUI is running. `shutdown` stops transfers and closes the engine cleanly;
the next command restores the saved torrents and pause states.

For foreground operation, run `./gotorrent serve` and stop it with Ctrl+C or
SIGTERM. Do not run a second daemon for the same state directory.

Defaults live under the OS user configuration directory (`$XDG_CONFIG_HOME` or
`~/.config` on Linux, `~/Library/Application Support` on macOS), in `gotor-cli/`:

- `downloads/`: data, isolated by torrent ID
- torrent state and metainfo: restored on startup
- `daemon.sock`: owner-only local control socket
- `daemon.log`: structured JSON logs from an automatically started daemon

Override locations with global flags:

```sh
./gotorrent --state-dir /tmp/my-gotor --data-dir "$HOME/Downloads/torrents" ui
./gotorrent --state-dir /tmp/my-gotor list --json
./gotorrent --state-dir /tmp/my-gotor shutdown
```

`--data-dir` is used when starting the daemon; an already-running daemon retains
its existing download directory. Always use the same data directory when
restarting a given state directory. Keep the state directory path short enough
for your OS's Unix-socket path limit (typically around 100 bytes).

JSON goes to stdout, errors to stderr, and failed commands return a nonzero
exit status. Sizes are bytes, rates are bytes/second, progress is a fraction
from 0 to 1, and ETA is seconds (`-1` means unknown).

### TUI keys

| Key | Action |
| --- | --- |
| ↑ / ↓ or j / k | Select torrent |
| Enter | Open/close expanded details |
| a | Add magnet URI or local torrent path |
| p | Pause/resume selected torrent |
| d | Confirm removal (data preserved by default) |
| ? | Toggle help |
| Esc | Cancel input/dialog |
| q / Ctrl+C | Quit the TUI |

The main screen shows aggregate rates and peer counts, a torrent table, and
selected-torrent details including files. Refreshes and actions run asynchronously
so input stays responsive. Resize the terminal freely; small windows use a compact
layout.

## Architecture

- `internal/app`: application-owned models and service contract; no Cobra or Tea
  dependencies.
- `internal/torrent`: anacrolix engine, snapshots, and persistent state.
- `internal/daemon`: local RPC server/client and daemon lifecycle.
- `internal/cli`: Cobra commands and script-friendly output.
- `internal/tui`: Bubble Tea v2 model, Bubbles widgets, and Lip Gloss layout.
- `internal/config`: storage configuration.
- `cmd/gotorrent`: signal-aware entry point.

Both frontends depend on the application service, not anacrolix types. The first
milestone deliberately omits advanced tracker management, bandwidth limits,
selective file downloads, and remote/network daemon access.
