//go:build !windows

package main

import (
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// runTarget replaces the current process with the target program via exec().
// The PID (and thus the PID file) stays valid, and stop/reload signals reach
// the target directly with no forwarding layer.
//
// In the plain-daemon role stdout/stderr are redirected to <pidfile>.log
// first; in the watchdog-worker role they are already piped to the watchdog
// log, so they are left alone.
func runTarget(target []string) {
	prog, err := exec.LookPath(target[0])
	if err != nil {
		log.Fatalf("daemonize: cannot find program %q: %v", target[0], err)
	}

	if pf := os.Getenv(pidFileEnvVar); pf != "" && os.Getenv(workerEnvVar) != "1" {
		logPath := strings.TrimSuffix(pf, ".pid") + ".log"
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
			dup2(int(f.Fd()), 1)
			dup2(int(f.Fd()), 2)
		}
	}

	// Strip our role markers so the target doesn't inherit them.
	env := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, pidFileEnvVar+"=") ||
			strings.HasPrefix(e, watchdogEnvVar+"=") ||
			strings.HasPrefix(e, workerEnvVar+"=") {
			continue
		}
		env = append(env, e)
	}

	// argv[0] is the resolved path so restart can re-launch it from /proc.
	argv := append([]string{prog}, target[1:]...)
	if err := syscall.Exec(prog, argv, env); err != nil {
		log.Fatalf("daemonize: failed to exec %q: %v", prog, err)
	}
}
