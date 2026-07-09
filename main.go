// daemonize runs any command-line program in the background as a daemon,
// with PID-file management and an optional auto-restarting watchdog.
// Daemonizing logic adapted from github.com/SubhashBose/RouteMUX/daemon.
//
// Usage:
//
//	daemonize start        <program> [args...]   run program as a daemon
//	daemonize watch-start  <program> [args...]   daemon + watchdog auto-restart
//	daemonize stop         <program>             stop the running daemon
//	daemonize restart      <program>             stop, then start with the same args
//	daemonize reload       <program>             send SIGHUP to the daemon
//	daemonize status       <program>             show status and recent log
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SubhashBose/daemonize/daemon"
)

// Role-marker env vars, must match the daemon package's internal constants.
const (
	pidFileEnvVar  = "DAEMON_PID_FILE"
	watchdogEnvVar = "DAEMON_IS_WATCHDOG"
	workerEnvVar   = "DAEMON_IS_WORKER"
)

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: daemonize <command> [options] <program> [args...]

Commands:
  start        run program detached in the background as a daemon
  watch-start  like start, but a watchdog restarts the program if it crashes
  stop         stop the running daemon (SIGTERM)
  restart      stop, then start again with the same arguments
  reload       send SIGHUP to the running daemon
  status       show whether the daemon is running and tail its log

Options (for watch-start):
  --restart-on-clean-exit        also restart the program when it exits with
                                 status 0 (default: restart on error only)
  --watch-restart-delay <dur>    delay before the watchdog restarts the
                                 program, e.g. 500ms, 5s, 1m, or a plain
                                 number of seconds (default: 2s)
  --job-name <name>              Set a name for the job, which will be used
                                 for job control from anywhere (default: "")
`)
	os.Exit(2)
}

// options are the wrapper's own flags, given between the control command and
// the target program. They stay in the passthrough args when the daemon
// re-execs itself, so every role (watchdog, worker, plain daemon) re-parses
// them here — that is also what preserves them across 'restart'.
type options struct {
	restartOnCleanExit bool
	watchRestartDelay  time.Duration
	jobName            string
}

func parseOptions(args []string) (options, []string) {
	var o options
	for len(args) > 0 {
		arg, val := args[0], ""
		hasVal := false
		if i := strings.Index(arg, "="); i >= 0 && strings.HasPrefix(arg, "--") {
			arg, val, hasVal = arg[:i], arg[i+1:], true
		}
		switch arg {
		case "--restart-on-clean-exit":
			if hasVal {
				fmt.Fprintf(os.Stderr, "daemonize: %s does not take a value\n", arg)
				os.Exit(2)
			}
			o.restartOnCleanExit = true
			args = args[1:]
		case "--watch-restart-delay":
			if !hasVal {
				if len(args) < 2 {
					fmt.Fprintf(os.Stderr, "daemonize: %s requires a value\n", arg)
					os.Exit(2)
				}
				val = args[1]
				args = args[2:]
			} else {
				args = args[1:]
			}
			d, err := parseDelay(val)
			if err != nil || d <= 0 {
				fmt.Fprintf(os.Stderr, "daemonize: invalid %s value %q (use e.g. 500ms, 5s, 1m)\n", arg, val)
				os.Exit(2)
			}
			o.watchRestartDelay = d
		case "--job-name":
			if !hasVal {
				if len(args) < 2 {
					fmt.Fprintf(os.Stderr, "daemonize: %s requires a value\n", arg)
					os.Exit(2)
				}
				o.jobName = args[1]
				args = args[2:]
			} else {
				args = args[1:]
			}
		default:
			return o, args
		}
	}
	return o, args
}

// parseDelay accepts Go duration syntax ("500ms", "5s") or a bare number of seconds.
func parseDelay(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return time.ParseDuration(s)
}

func main() {
	var target []string

	// In re-exec'd child roles (daemon / watchdog / worker) the control
	// command has already been stripped, so os.Args[1:] is the target command.
	childRole := os.Getenv(pidFileEnvVar) != "" ||
		os.Getenv(watchdogEnvVar) == "1" ||
		os.Getenv(workerEnvVar) == "1"

	if childRole {
		target = os.Args[1:]
	} else {
		if len(os.Args) < 3 {
			usage()
		}
		switch os.Args[1] {
		case "start", "watch-start", "stop", "restart", "reload", "status":
		default:
			fmt.Fprintf(os.Stderr, "daemonize: unknown command %q\n\n", os.Args[1])
			usage()
		}
		target = os.Args[2:]
	}

	opts, target := parseOptions(target)
	if len(target) == 0 {
		if opts.jobName == "" {
			fmt.Fprintf(os.Stderr, "daemonize: missing target program\n\n")
			usage()
		}
		switch os.Args[1] {
		case "start", "watch-start":
			fmt.Fprintf(os.Stderr, "daemonize: missing target program\n\n")
			usage()
		default:
			target = []string{"Job " + opts.jobName}
		}
	}

	var hashKey string
	var pidfilePrefix string
	if opts.jobName == "" {
		// If the job name is not specified, use the target program path.
		// The PID file is keyed on uid + cwd + the resolved target program path
		// (same hashing scheme as RouteMUX), so each program gets its own daemon
		// per directory, and stop/status only need the program name.
		progPath := target[0]
		if p, err := exec.LookPath(target[0]); err == nil {
			if abs, err := filepath.Abs(p); err == nil {
				progPath = abs
			}
		}
		cwd, _ := os.Getwd()
		hashKey = cwd + "|" + progPath
		pidfilePrefix = filepath.Base(target[0])
	} else {
		hashKey = opts.jobName
		pidfilePrefix = opts.jobName
	}
	daemon.Handle(daemon.Config{
		AppName:       filepath.Base(target[0]),
		PidfilePrefix: pidfilePrefix,
		HashKey:       hashKey,
		// HashSalt left empty: the daemon package defaults it to this binary's
		// module path, which namespaces our PID files from other programs
		// that import the package.
		RestartOnCleanExit:   opts.restartOnCleanExit,
		WatchdogRestartDelay: opts.watchRestartDelay,
		OnStart:              func() { runTarget(target) },
	})
}
