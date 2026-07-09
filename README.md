# daemonize

Run any command-line program in the background as a daemon, with PID-file
management, status/stop/restart control, and an optional auto-restarting
watchdog. The daemonizing logic is adapted from
[RouteMUX/daemon](https://github.com/SubhashBose/RouteMUX/tree/master/daemon).

## Install

```sh
go install github.com/SubhashBose/daemonize@latest
```

Or build from a clone:

```sh
go build -o daemonize .
```

## Usage

```
daemonize <start|watch-start|stop|restart|reload|status> [options] <program> [args...]
```

Options (given before the program, relevant to `watch-start`):

| Option | What it does |
|---|---|
| `--restart-on-clean-exit` | Also restart the program when it exits with status 0. By default the watchdog only restarts on error and treats a clean exit as an intentional stop. |
| `--watch-restart-delay <dur>` | Delay before the watchdog restarts the program. Accepts Go durations (`500ms`, `5s`, `1m`) or a bare number of seconds; `--watch-restart-delay=5s` also works. Default: 2s. |

Options are preserved across `restart`, since restart re-reads the running
process's full command line.

### Restarting a program that provides your terminal

`restart` runs its stop+start sequence in a process detached from the
controlling terminal (a new session via `setsid`). This matters when the
program you are restarting is what provides the terminal you issued the
command from — e.g. a terminal server. Stopping it tears down that terminal,
which would otherwise kill the `restart` process *after* the stop but *before*
the start, leaving the program down.

With the detached restart, the terminal dying kills only the foreground
command; the detached copy keeps running in its own session and completes the
start. When the terminal survives (the normal case), the foreground waits on
the detached copy, so output stays synchronous and the command blocks until
the restart is done — the detachment is invisible.

| Command | What it does |
|---|---|
| `start prog args...` | Fork, detach from the terminal (setsid), then `exec()` the program. The PID file holds the program's real PID. stdout/stderr go to `<pidfile>.log`. |
| `watch-start prog args...` | Same, but a detached watchdog process supervises the program: restarts it (after 2s) if it exits non-zero, and shuts down if it exits cleanly. Program output is captured in the watchdog log. |
| `stop prog` | SIGTERM the daemon (or watchdog, which forwards it to the program), wait for exit, remove the PID file. |
| `restart prog` | Stop, then start again in the same mode with the same arguments (read from the running process via `/proc`). |
| `reload prog` | Send SIGHUP to the program (the watchdog forwards it). |
| `status prog` | Show running/stopped, PID, command line, and the last 10 log lines. Cleans up stale PID files. |

Examples:

```sh
./daemonize start ./myserver --port 8080
./daemonize status ./myserver
./daemonize reload ./myserver          # SIGHUP, e.g. to re-read config
./daemonize watch-start python3 worker.py
./daemonize watch-start --restart-on-clean-exit --watch-restart-delay 10s ./batchjob
./daemonize restart python3            # same args as the running instance
./daemonize stop python3
```

## PID files

Same scheme as RouteMUX: `$TMPDIR/<prog>-<md5>.pid`, where the hash is
`md5(uid + hashkey + daemonize-exe-path)` and the hash key is
`cwd + "|" + resolved-program-path`. So there is one daemon slot per
(user, working directory, program), and `stop`/`status`/`restart` only need
the program name — run them from the same directory you started from.
The PID file's second line records the mode (`start` / `watch-start`) so
`restart` knows which mode to relaunch in.

## Reusing the daemon package

The daemonizing logic lives in an importable package:

```go
import "github.com/SubhashBose/daemonize/daemon"
```

It handles start/stop/watch-start/restart/reload/status, PID-file management,
and graceful shutdown for any Go program — put your program logic in
`Config.OnStart`. PID files are namespaced per application automatically: the
salt defaults to the importing binary's module path, so two programs using
this package won't collide on the same job or program name. Override
`Config.HashSalt` to set it explicitly.

## Layout

- `daemon/` — the importable daemonizing package, adapted from RouteMUX with
  small changes: the control command must be the first argument (so target
  args like `stop` are never misparsed), `stop` removes the PID file (an
  exec()ed target can't), and `restart` understands that a plain daemon's
  argv *is* the target command.
- module root (`main.go`, `target_unix.go`, …) — the CLI wrapper; in the final
  child role it `exec()`s the target program so signals and the PID file refer
  to the target itself.

Unix only; on Windows it runs the program attached to the terminal
(matching the upstream package's behavior).
