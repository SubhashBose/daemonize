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
// Output redirection is handled by the daemon package: in the plain-daemon
// role it redirects stdout/stderr to <pidfile>.log (DaemonOutputLog) before
// calling this, and that redirection is inherited across the exec() below; in
// the watchdog-worker role output is piped to the watchdog log.
func runTarget(target []string) {
	prog, err := exec.LookPath(target[0])
	if err != nil {
		log.Fatalf("daemonize: cannot find program %q: %v", target[0], err)
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
