//go:build !windows

// Package daemon provides daemonizing functionality, adapted from
// github.com/SubhashBose/RouteMUX/daemon.
//
// It handles start/stop/status commands, PID file management, and graceful
// shutdown. It remains API- and behavior-compatible with the RouteMUX
// original (which self-daemonizes a single Go program) when used with the
// zero-value Config: the control command is found by scanning from the end,
// so it may follow the program's own flags.
//
// The package additionally supports wrapping an arbitrary external program
// (see the daemonize command). Behaviors relevant to that mode:
//   - Config.CommandMustBeFirst restricts the control command to the first
//     argument, so a wrapped program's own arguments (which may themselves be
//     words like "stop") are never misparsed;
//   - restart re-reads the running process's full cmdline from /proc, taking
//     into account that a plain daemon may have exec()ed into the target
//     program (its argv IS the target command);
//   - stop removes the PID file itself, since an exec()ed target cannot;
//   - status/start also know about the plain-daemon log file
//     (<pidfile>.log) that the wrapper redirects target output into.
package daemon

import (
	"bufio"
	"crypto/md5"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const DAEMONIZE_SUPPORTED = true

// Config holds optional settings for the daemon.
type Config struct {
	// OnStart is called in the daemon process after it has started.
	// Put your main program logic here.
	OnStart func()

	// AppName is the name of the application.
	// Defaults to the basename of the current executable.
	AppName string

	// PidfilePrefix is the prefix for the PID file.
	// Defaults to the basename of the current executable.
	PidfilePrefix string

	//HashKey to be used for the PID filename. Default is current working directory
	HashKey string

	//HashSalt to be used for the PID filename. Default is a fixed arbitrary value
	HashSalt string

	// WaitAfterStart is how long to wait after forking before confirming the
	// daemon is still alive. Defaults to 500ms.
	WaitAfterStart time.Duration

	// WatchdogRestartDelay is how long the watchdog waits before restarting a worker.
	// Defaults to 2s.
	WatchdogRestartDelay time.Duration

	// Logger file to used for daemon-internal messages. Logging defaults to log.Default().
	LoggerFile string

	// WatchdogLogger file to used for watchdog messages.
	// Defaults to same as LoggerFile if it is set, otherwise log file is PID-file basename with "-watchdog.log".
	WatchdogLoggerFile string

	// DaemonOutputLog, when true, redirects a plain daemon's stdout and stderr
	// to a per-daemon output log at <pidfile>.log before running OnStart. The
	// redirection is done at the file-descriptor level, so it is inherited by
	// any program the daemon exec()s. 'start' then reports the log's path and
	// 'status' tails it. Has no effect in watch-start mode, whose output is
	// captured by the watchdog log instead. Default false.
	DaemonOutputLog bool

	// Restart (when watch-start) worker on clean exit too. Default is restart on error only
	// Default value false.
	RestartOnCleanExit bool

	// Command strings to be used for start/stop/restart/status commands.
	// Defaults to "start", "watch-start", "stop", "restart", "reload", "status".
	CommandStrings Commands

	// CommandMustBeFirst controls where the control command may appear in the
	// arguments.
	//
	// Default (false): the command is found by scanning from the end, so it may
	// follow the program's own flags, e.g. "./app --config c.yml start". This
	// suits a self-daemonizing program whose only arguments are its own.
	//
	// True: the command is only recognized as the first argument, e.g.
	// "./app start --config c.yml". This is required when the arguments after
	// the command belong to an arbitrary wrapped program, since those may
	// themselves contain words like "stop".
	CommandMustBeFirst bool

	// Log and PID file modtime refresh interval to avoid OS cleaning up /tmp.
	// Defaults to 12h.
	TmpFileTouchInterval time.Duration

	// (internal variables) Logger is used for daemon-internal messages. Defaults to log.Default().
	logger *log.Logger

	// (internal) path of PID file.
	pidFile string
}

type Commands struct {
	Start      string
	WatchStart string
	Stop       string
	Restart    string
	Reload     string
	Status     string
}

const pidFileEnvVar = "DAEMON_PID_FILE"
const watchdogEnvVar = "DAEMON_IS_WATCHDOG"
const workerEnvVar = "DAEMON_IS_WORKER"

// Marks the detached process that actually performs a restart's stop+start,
// so it isn't re-detached again. See handleRestart.
const restartDetachedEnvVar = "DAEMON_RESTART_DETACHED"

// set to true when watchdog in shutting down state
var watchdogShuttingDown atomic.Bool

// Handle inspects os.Args for control commands and acts accordingly.
//
// Commands:
//
//	daemonize start prog [args]        — daemonize prog
//	daemonize watch-start prog [args]  — daemonize prog with watchdog auto-restart
//	daemonize stop prog                — kill the running daemon or watchdog
//	daemonize restart prog             — stop then start again (same args as running instance)
//	daemonize reload prog              — send SIGHUP to the daemon
//	daemonize status prog              — print whether the daemon is running
func Handle(cfg Config) {

	setBasicDefaults := func() {
		if cfg.TmpFileTouchInterval == 0 {
			cfg.TmpFileTouchInterval = 12 * time.Hour
		}
		if cfg.AppName == "" {
			exe, _ := os.Executable()
			cfg.AppName = filepath.Base(exe)
		}
		if cfg.PidfilePrefix == "" {
			exe, _ := os.Executable()
			cfg.PidfilePrefix = filepath.Base(exe)
		}
		if cfg.HashSalt == "" {
			cfg.HashSalt = defaultHashSalt()
		}
		if cfg.WaitAfterStart == 0 {
			cfg.WaitAfterStart = 500 * time.Millisecond
		}
		if cfg.WatchdogRestartDelay == 0 {
			cfg.WatchdogRestartDelay = 2 * time.Second
		}
		// Always have a valid logger as fallback
		cfg.logger = log.Default()
	}

	initLogger := func() {
		if cfg.LoggerFile != "" {
			f, err := os.OpenFile(cfg.LoggerFile, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0644)
			if err == nil {
				cfg.logger = log.New(f, "", log.LstdFlags)
				refreshModTime(cfg.LoggerFile, cfg.TmpFileTouchInterval)
			}
		}
	}

	// Role: plain worker (child of watchdog) — just run OnStart, no PID file handling.
	if os.Getenv(workerEnvVar) == "1" {
		log.SetFlags(log.Flags() &^ (log.Ldate | log.Ltime))
		if cfg.OnStart != nil {
			cfg.OnStart()
		}
		return
	}

	// Role: watchdog daemon — monitor and restart the worker.
	if os.Getenv(watchdogEnvVar) == "1" {
		setBasicDefaults()
		cfg.pidFile = os.Getenv(pidFileEnvVar)
		cfg.AppName = cfg.AppName + " watchdog"
		refreshModTime(cfg.pidFile, cfg.TmpFileTouchInterval)
		runWatchdog(&cfg)
		return
	}

	// Role: plain daemon — set up graceful shutdown and run OnStart.
	if pidFile := os.Getenv(pidFileEnvVar); pidFile != "" {
		setBasicDefaults()
		cfg.pidFile = pidFile
		if cfg.DaemonOutputLog {
			logf := getDaemonLogfileName(&cfg)
			if err := redirectStdOutErr(logf); err != nil {
				cfg.logger.Printf("%s: failed to open output log %s: %v", cfg.AppName, logf, err)
			} else {
				refreshModTime(logf, cfg.TmpFileTouchInterval)
			}
		}
		initLogger()
		setupGracefulShutdown(&cfg, nil)
		refreshModTime(cfg.pidFile, cfg.TmpFileTouchInterval)
		if cfg.OnStart != nil {
			cfg.OnStart()
		}
		return
	}

	if cfg.CommandStrings == (Commands{}) {
		cfg.CommandStrings.Start = "start"
		cfg.CommandStrings.WatchStart = "watch-start"
		cfg.CommandStrings.Stop = "stop"
		cfg.CommandStrings.Restart = "restart"
		cfg.CommandStrings.Reload = "reload"
		cfg.CommandStrings.Status = "status"
	}

	// Role: parent — parse command and act.
	command, passArgs := parseArgs(os.Args[1:], &cfg.CommandStrings, cfg.CommandMustBeFirst)

	handlers := map[string]func(){
		cfg.CommandStrings.Start:      func() { handleStart(passArgs, &cfg) },
		cfg.CommandStrings.WatchStart: func() { handleWatchStart(passArgs, &cfg) },
		cfg.CommandStrings.Stop:       func() { _ = handleStop(&cfg) },
		cfg.CommandStrings.Restart:    func() { handleRestart(passArgs, &cfg) },
		cfg.CommandStrings.Reload:     func() { handleReload(&cfg) },
		cfg.CommandStrings.Status:     func() { handleStatus(&cfg) },
	}
	if handler, ok := handlers[command]; command != "" && ok {
		setBasicDefaults()
		initLogger()
		pidFile, err := pidFilePath(cfg)
		if err != nil {
			cfg.logger.Fatalf("%s daemon: cannot determine PID file path: %v", cfg.AppName, err)
		}
		cfg.pidFile = pidFile

		handler()
	} else {
		if cfg.OnStart != nil {
			cfg.OnStart()
		}
	}
}

// ---- internal ---------------------------------------------------------------

// parseArgs separates the control command from the rest of the arguments.
// When mustBeFirst is true the command is only recognized as args[0] (so an
// arbitrary wrapped program's own arguments are passed through untouched);
// otherwise it is found by scanning from the end (so it may follow the
// program's own flags).
func parseArgs(args []string, commandStrings *Commands, mustBeFirst bool) (command string, rest []string) {
	validCommands := make(map[string]struct{})
	for _, cmd := range []string{
		commandStrings.Start,
		commandStrings.WatchStart,
		commandStrings.Stop,
		commandStrings.Restart,
		commandStrings.Reload,
		commandStrings.Status,
	} {
		if cmd != "" {
			validCommands[cmd] = struct{}{}
		}
	}

	if mustBeFirst {
		if len(args) > 0 {
			if _, ok := validCommands[args[0]]; ok {
				return args[0], args[1:]
			}
		}
		return "", args
	}

	// Scan from the end: the last matching argument wins.
	for i := len(args) - 1; i >= 0; i-- {
		if _, ok := validCommands[args[i]]; ok {
			rest = make([]string, 0, len(args)-1)
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+1:]...)
			return args[i], rest
		}
	}
	return "", args
}

// defaultHashSalt derives a per-application salt so that different programs
// importing this package don't collide on PID file names when they share a
// job or program name. It uses the main module path of the final binary —
// which is always the importing application, not this package — falling back
// to a fixed constant for non-module builds where build info is unavailable.
// Callers can override it by setting Config.HashSalt explicitly.
func defaultHashSalt() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Path != "" {
		return bi.Main.Path
	}
	return "5Ud@wsPAoTXLbAWdvDC6"
}

func pidFilePath(cfg Config) (string, error) {
	if cfg.HashKey == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		cfg.HashKey, _ = filepath.Abs(cwd)

		exe, err := os.Executable()
		if err != nil {
			exe = ""
		} else {
			exe, _ = filepath.Abs(exe)
		}
		cfg.HashKey = fmt.Sprintf("%s%s", cfg.HashKey, exe)
	}
	uid := os.Getuid()
	raw := fmt.Sprintf("%d%s%s", uid, cfg.HashKey, cfg.HashSalt)
	hash := fmt.Sprintf("%x", md5.Sum([]byte(raw)))
	return filepath.Join(os.TempDir(), cfg.PidfilePrefix+"-"+hash+".pid"), nil
}

// forkDaemon launches exe with the given args and env as a detached daemon.
// It waits up to cfg.WaitAfterStart to confirm it didn't crash immediately.
// Returns the PID of the started process.
func forkDaemon(exe string, args []string, env []string, cfg *Config) int {
	cmd := exec.Command(exe, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach from terminal
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		cfg.logger.Fatalf("%s: failed to fork process: %v", cfg.AppName, err)
	}

	pid := cmd.Process.Pid

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case err := <-exited:
		if err != nil {
			cfg.logger.Fatalf("%s: process exited immediately: %v", cfg.AppName, err)
		} else {
			cfg.logger.Fatalf("%s: process exited immediately with no error", cfg.AppName)
		}
	case <-time.After(cfg.WaitAfterStart):
		// still alive
	}

	cmd.Process.Release()
	return pid
}

func handleStart(childArgs []string, cfg *Config) {
	if pid, _, _, _, err := readPID(cfg.pidFile); err == nil {
		if processExists(pid) {
			fmt.Printf("%s already running (PID %d). Use 'stop' first.\n", cfg.AppName, pid)
			os.Exit(0)
		}
		os.Remove(cfg.pidFile)
	}

	if wg_logf := getWatchdogLogfileName(cfg); cfg.LoggerFile != wg_logf && fileExists(wg_logf) {
		os.Remove(wg_logf)
	}

	exe, _ := filepath.Abs(mustExecutable())

	env := append(os.Environ(), pidFileEnvVar+"="+cfg.pidFile)
	pid := forkDaemon(exe, childArgs, env, cfg)

	if err := writePID(cfg.pidFile, pid, cfg.AppName, "start", childArgs); err != nil {
		cfg.logger.Fatalf("%s: failed to write PID file: %v", cfg.AppName, err)
	}
	fmt.Printf("%s daemon started (PID %d)\n", cfg.AppName, pid)
	if cfg.DaemonOutputLog {
		fmt.Printf("Output log: %s\n", getDaemonLogfileName(cfg))
	}
}

func handleWatchStart(childArgs []string, cfg *Config) {
	if pid, _, _, _, err := readPID(cfg.pidFile); err == nil {
		if processExists(pid) {
			fmt.Printf("%s already running (PID %d). Use 'stop' first.\n", cfg.AppName, pid)
			os.Exit(0)
		}
		os.Remove(cfg.pidFile)
	}

	if wg_logf := getWatchdogLogfileName(cfg); cfg.LoggerFile != wg_logf && fileExists(wg_logf) {
		os.Remove(wg_logf)
	}

	exe, _ := filepath.Abs(mustExecutable())

	env := append(os.Environ(),
		pidFileEnvVar+"="+cfg.pidFile,
		watchdogEnvVar+"=1",
	)
	pid := forkDaemon(exe, childArgs, env, cfg)

	if err := writePID(cfg.pidFile, pid, cfg.AppName, "watch-start", childArgs); err != nil {
		cfg.logger.Fatalf("%s: failed to write PID file: %v", cfg.AppName, err)
	}
	fmt.Printf("%s watchdog started (PID %d)\n", cfg.AppName, pid)
	fmt.Printf("Watchdog log: %s\n", getWatchdogLogfileName(cfg))
}

func getWatchdogLogfileName(cfg *Config) string {
	if cfg.WatchdogLoggerFile != "" {
		return cfg.WatchdogLoggerFile
	}
	if cfg.LoggerFile != "" {
		return cfg.LoggerFile
	}
	return strings.TrimSuffix(cfg.pidFile, ".pid") + "-watchdog.log"
}

// getDaemonLogfileName is the log file the plain (non-watchdog) daemon
// redirects its stdout/stderr into (see DaemonOutputLog).
func getDaemonLogfileName(cfg *Config) string {
	return strings.TrimSuffix(cfg.pidFile, ".pid") + ".log"
}

// redirectStdOutErr points the process's stdout and stderr (fds 1 and 2) at
// the given file, creating it if needed and appending otherwise. Because it
// operates at the file-descriptor level, the redirection survives exec(), so
// output of a wrapped target program is captured too.
func redirectStdOutErr(path string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close() // fds 1 and 2 keep the file open after the original fd closes
	if err := dup2(int(f.Fd()), 1); err != nil {
		return err
	}
	return dup2(int(f.Fd()), 2)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}

// runWatchdog is the watchdog loop. It runs inside the detached watchdog process,
// repeatedly spawning the worker and restarting it if it crashes.
func runWatchdog(cfg *Config) {
	exe, _ := filepath.Abs(mustExecutable())

	cfg.WatchdogLoggerFile = getWatchdogLogfileName(cfg)

	f, err := os.OpenFile(cfg.WatchdogLoggerFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		cfg.logger = log.New(f, "", log.LstdFlags)
		cfg.LoggerFile = cfg.WatchdogLoggerFile
		cfg.logger.Printf("%s: started logging to %s", cfg.AppName, cfg.WatchdogLoggerFile)
		refreshModTime(cfg.WatchdogLoggerFile, cfg.TmpFileTouchInterval)
	}

	// Build a clean env for the worker: strip watchdog/pidfile markers, add worker marker.
	baseEnv := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, watchdogEnvVar+"=") ||
			strings.HasPrefix(e, pidFileEnvVar+"=") {
			continue
		}
		baseEnv = append(baseEnv, e)
	}
	workerEnv := append(baseEnv, workerEnvVar+"=1")

	// currentWorker is updated each restart so the signal handler can always
	// reach the live worker process.
	var currentWorker *exec.Cmd
	setupGracefulShutdown(cfg, &currentWorker)
	forwardSignal(cfg, &currentWorker)

	attempt := 0
	for {
		attempt++
		cfg.logger.Printf("%s: starting worker (attempt %d)", cfg.AppName, attempt)

		cmd := exec.Command(exe, os.Args[1:]...)
		cmd.Env = workerEnv
		// No Setsid — worker is a plain child of the watchdog.
		cmd.Stdin = nil

		// pipeToLogger connects an io.Reader to a logger, prefixing each line.
		pipeToLogger := func(r io.Reader, logger *log.Logger, prefix string) {
			scanner := bufio.NewScanner(r)
			for scanner.Scan() {
				logger.Printf("%s %s", prefix, scanner.Text())
			}
		}
		// Connect stdout and stderr pipes to the logger
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			cfg.logger.Printf("%s: failed to create stdout pipe: %v", cfg.AppName, err)
		} else {
			go pipeToLogger(stdoutPipe, cfg.logger, "Worker [stdout]:")
		}

		stderrPipe, err := cmd.StderrPipe()
		if err != nil {
			cfg.logger.Printf("%s: failed to create stderr pipe: %v", cfg.AppName, err)
		} else {
			go pipeToLogger(stderrPipe, cfg.logger, "Worker [stderr]:")
		}

		if err := cmd.Start(); err != nil {
			cfg.logger.Printf("%s: failed to start worker: %v — retrying in %s",
				cfg.AppName, err, cfg.WatchdogRestartDelay)
			time.Sleep(cfg.WatchdogRestartDelay)
			continue
		}

		currentWorker = cmd
		err = cmd.Wait() // blocks until worker exits
		currentWorker = nil

		if watchdogShuttingDown.Load() {
			select {} // halting, allowing setupGracefulShutdown() to shutdown
		}

		if err != nil || cfg.RestartOnCleanExit {
			status_msg := "exited cleanly"
			if err != nil {
				status_msg = fmt.Sprintf("crashed (%v)", err)
			}
			cfg.logger.Printf("%s: worker %s — restarting in %s",
				cfg.AppName, status_msg, cfg.WatchdogRestartDelay)
			time.Sleep(cfg.WatchdogRestartDelay)
		} else {
			// Clean exit (exit code 0) means intentional stop — watchdog exits too.
			cfg.logger.Printf("%s: worker exited cleanly, shutting down", cfg.AppName)
			os.Remove(cfg.pidFile)
			os.Exit(0)
		}
	}
}

func handleStop(cfg *Config) bool {
	pid, appname, _, _, err := readPID(cfg.pidFile)
	if appname != "" {
		cfg.AppName = appname
	}
	if err != nil {
		fmt.Printf("%s is not running.\n", cfg.AppName)
		return false
	}
	if !processExists(pid) {
		fmt.Printf("%s is not running (stale PID %d). Cleaning up.\n", cfg.AppName, pid)
		os.Remove(cfg.pidFile)
		return false
	}

	proc, _ := os.FindProcess(pid)
	fmt.Printf("Sending SIGTERM to %s (PID %d)...\n", cfg.AppName, pid)
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		os.Remove(cfg.pidFile)
		cfg.logger.Fatalf("%s: failed to signal process: %v", cfg.AppName, err)
	}

	// Wait for the process to actually exit.
	for processExists(pid) {
		time.Sleep(100 * time.Millisecond)
	}
	// An exec()ed target cannot clean up its own PID file, so do it here.
	os.Remove(cfg.pidFile)
	fmt.Println("Stopped.")
	return true
}

func handleRestart(fallbackArgs []string, cfg *Config) {
	// Detach the restart from the controlling terminal on first entry. If the
	// program we are about to stop is itself what provides this terminal
	// (e.g. a terminal server the user is connected through), stopping it will
	// tear the terminal down and kill this process — after the stop half has
	// run but before the start half. Re-exec ourselves once in a new session
	// so the whole stop+start sequence completes regardless.
	//
	// The foreground process waits on the detached child, so when the terminal
	// survives (the normal case) output stays synchronous and the command
	// blocks until the restart is done. When the terminal dies, the foreground
	// is killed but the detached child — in its own session — finishes anyway.
	pid, appname, mode, storedArgs, _ := readPID(cfg.pidFile)
	if appname != "" {
		cfg.AppName = appname
	}

	if os.Getenv(restartDetachedEnvVar) != "1" {
		exe, _ := filepath.Abs(mustExecutable())
		cmd := exec.Command(exe, os.Args[1:]...)
		cmd.Env = append(os.Environ(), restartDetachedEnvVar+"=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // escape the terminal's session
		cmd.Stdin = nil
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			cfg.logger.Fatalf("%s: failed to launch detached restart: %v", cfg.AppName, err)
		}
		_ = cmd.Wait()
		return
	}
	// Don't leak the marker into the restarted daemon or the target program.
	os.Unsetenv(restartDetachedEnvVar)

	if mode == "" {
		mode = "start" // fallback
	}

	// Relaunch with the exact command the daemon was originally started with.
	// We use the command recorded in the PID file rather than /proc/<pid>/cmdline,
	// because the running process may have exec()ed into a different program
	// (e.g. `bash -c "... ; sleep 500"` tail-calls into sleep), which would
	// otherwise make us restart only that inner program.
	passArgs := fallbackArgs
	if len(storedArgs) > 0 {
		passArgs = storedArgs
	} else if pid != 0 {
		// Backward compatibility: PID files written before the command was
		// recorded. Fall back to /proc (imperfect for the exec case above).
		if args, err := getProcessArgs(pid); err == nil {
			if mode == "watch-start" {
				passArgs = args[1:]
			} else {
				passArgs = args
			}
		}
	}

	if handleStop(cfg) {
		cfg.WatchdogLoggerFile = getWatchdogLogfileName(cfg) + " " // making file nonexistent, tricking to not delete the file on restart
		switch mode {
		case "watch-start":
			handleWatchStart(passArgs, cfg)
		default:
			handleStart(passArgs, cfg)
		}
	}
}

func handleReload(cfg *Config) {
	pid, appname, _, _, err := readPID(cfg.pidFile)
	if appname != "" {
		cfg.AppName = appname
	}
	if err != nil {
		fmt.Printf("%s is not running.\n", cfg.AppName)
		return
	}
	if !processExists(pid) {
		fmt.Printf("%s is not running (stale PID %d). Cleaning up.\n", cfg.AppName, pid)
		os.Remove(cfg.pidFile)
		return
	}
	proc, _ := os.FindProcess(pid)
	fmt.Printf("Sending SIGHUP to %s (PID %d)...\n", cfg.AppName, pid)
	if err := proc.Signal(syscall.SIGHUP); err != nil {
		cfg.logger.Fatalf("%s: failed to signal process: %v", cfg.AppName, err)
	}
}

func handleStatus(cfg *Config) {
	if wg_logf := getWatchdogLogfileName(cfg); fileExists(wg_logf) {
		fmt.Printf("Watchdog log: %s\n", wg_logf)
		tailFile(wg_logf, 10)
	} else if d_logf := getDaemonLogfileName(cfg); cfg.DaemonOutputLog && fileExists(d_logf) {
		fmt.Printf("Output log: %s\n", d_logf)
		tailFile(d_logf, 10)
	} else if cfg.LoggerFile != "" {
		fmt.Printf("Log file: %s\n", cfg.LoggerFile)
		tailFile(cfg.LoggerFile, 10)
	}

	pid, appname, mode, storedArgs, err := readPID(cfg.pidFile)
	if appname != "" {
		cfg.AppName = appname
	}
	if err != nil {
		fmt.Printf("Status: %s stopped\n", cfg.AppName)
		return
	}

	if mode == "watch-start" {
		mode = "watchdog"
	} else {
		mode = "daemon"
	}

	if processExists(pid) {
		fmt.Printf("Status: %s %s running (PID %d)\n", cfg.AppName, mode, pid)
		// Prefer the command the daemon was launched with (from the PID file);
		// the live /proc cmdline may differ if the process exec()ed into
		// another program. Fall back to /proc for old PID files.
		cmdArgs := storedArgs
		if len(cmdArgs) == 0 {
			if a, err := getProcessArgs(pid); err == nil {
				cmdArgs = a
			}
		}
		if len(cmdArgs) > 0 {
			fmt.Printf("Command: %s\n", strings.Join(cmdArgs, " "))
		}
	} else {
		fmt.Printf("Status: %s stopped\nBut process did not exit gracefully. Cleaning up.\n", cfg.AppName)
		os.Remove(cfg.pidFile)
	}
}

// setupGracefulShutdown registers SIGTERM/SIGINT handlers that optionally
// forward the signal to a worker process, remove the PID file, and exit.
// workerCmd may be nil (plain daemon) or a pointer to the current worker cmd
// (watchdog) — the pointer itself is stable but the cmd it points to changes
// each restart, so the handler always signals the live worker.
func setupGracefulShutdown(cfg *Config, workerCmd **exec.Cmd) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-ch
		cfg.logger.Printf("%s: received %s, shutting down...", cfg.AppName, sig)
		if workerCmd != nil && *workerCmd != nil && (*workerCmd).Process != nil {
			cfg.logger.Printf("%s: forwarding %s to worker (PID %d)",
				cfg.AppName, sig, (*workerCmd).Process.Pid)
			watchdogShuttingDown.Store(true)
			(*workerCmd).Process.Signal(sig.(syscall.Signal))

			// Wait for worker to exit, but don't wait forever.
			workerDone := make(chan struct{}, 1)
			go func() {
				(*workerCmd).Wait()
				close(workerDone)
			}()

			select {
			case <-workerDone:
				cfg.logger.Printf("%s: worker exited cleanly", cfg.AppName)
			case <-time.After(10 * time.Second):
				cfg.logger.Printf("%s: worker did not exit in time, forcing kill", cfg.AppName)
				(*workerCmd).Process.Kill()
			}
		}
		os.Remove(cfg.pidFile)
		os.Exit(0)
	}()
}

func forwardSignal(cfg *Config, workerCmd **exec.Cmd) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	go func() {
		for {
			sig := <-ch // block until signal arrives
			if workerCmd != nil && *workerCmd != nil && (*workerCmd).Process != nil {
				cfg.logger.Printf("%s: received %s, forwarding to worker (PID %d)",
					cfg.AppName, sig, (*workerCmd).Process.Pid)
				(*workerCmd).Process.Signal(sig.(syscall.Signal))
			} else {
				cfg.logger.Printf("%s: received %s but no worker is running", cfg.AppName, sig)
			}
		}
	}()
}

// ---- helpers ----------------------------------------------------------------

func mustExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("cannot determine executable path: %v", err)
	}
	return exe
}

// PID file format, one field per line:
//
//	<pid>
//	<appname>
//	<mode>
//	<arg0>\x00<arg1>\x00...   (the original command the daemon was launched with)
//
// The command args are NUL-separated — a byte that can never appear inside an
// exec argument — so spaces and shell metacharacters survive intact. Storing
// the launched command lets 'restart' relaunch it faithfully even after the
// daemon has exec()ed into the target (and the target into something else,
// e.g. bash tail-calling into sleep), which /proc/<pid>/cmdline no longer
// reflects.
func writePID(path string, pid int, appname, mode string, args []string) error {
	content := strconv.Itoa(pid) + "\n" + appname + "\n" + mode + "\n" + strings.Join(args, "\x00") + "\n"
	return os.WriteFile(path, []byte(content), 0644)
}

func readPID(path string) (pid int, appname, mode string, args []string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, "", "", nil, err
	}
	// Limit to 4 parts so the args blob keeps any embedded newlines; the fixed
	// fields are parsed from the first three lines.
	parts := strings.SplitN(string(data), "\n", 4)
	pid, err = strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, "", "", nil, err
	}
	if len(parts) > 1 {
		appname = strings.TrimSpace(parts[1])
	}
	mode = "start" // default for old PID files that don't record a mode
	if len(parts) > 2 {
		if m := strings.TrimSpace(parts[2]); m != "" {
			mode = m
		}
	}
	if len(parts) > 3 {
		if blob := strings.TrimSuffix(parts[3], "\n"); blob != "" {
			args = strings.Split(blob, "\x00")
		}
	}
	return pid, appname, mode, args, nil
}

// processExists checks whether a process with the given PID is alive.
func processExists(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks existence without actually sending a signal.
	return proc.Signal(syscall.Signal(0)) == nil
}

func getProcessArgs(pid int) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimRight(string(data), "\x00")
	if trimmed == "" {
		return nil, fmt.Errorf("process %d has no cmdline", pid)
	}
	return strings.Split(trimmed, "\x00"), nil
}

func tailFile(path string, n int) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("could not open file: %w", err)
	}
	defer f.Close()

	// Get file size
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("could not stat file: %w", err)
	}
	fileSize := info.Size()

	// Scan backwards to find n newlines
	bufSize := int64(4096)
	linesFound := 0
	offset := fileSize
	var startPos int64

	for offset > 0 && linesFound <= n {
		if offset < bufSize {
			bufSize = offset
		}
		offset -= bufSize

		buf := make([]byte, bufSize)
		_, err := f.ReadAt(buf, offset)
		if err != nil {
			return fmt.Errorf("could not read file: %w", err)
		}

		for i := len(buf) - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				linesFound++
				if linesFound == n {
					startPos = offset + int64(i) + 1
					break
				}
			}
		}
	}

	// Seek to start position and print
	_, err = f.Seek(startPos, io.SeekStart)
	if err != nil {
		return fmt.Errorf("could not seek file: %w", err)
	}

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fmt.Println("  ", scanner.Text())
	}
	fmt.Println()

	return scanner.Err()
}

// refreshModTime touches the file every interval to prevent
// the OS from cleaning it up from /tmp due to inactivity.
func refreshModTime(path string, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			if err := os.Chtimes(path, now, now); err != nil {
				// File may have been deleted externally — stop refreshing.
				continue
			}
		}
	}()
}
