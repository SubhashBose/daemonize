//go:build windows

package main

import (
	"log"
	"os"
	"os/exec"
)

// Daemonizing is not supported on Windows; run the target attached.
func runTarget(target []string) {
	cmd := exec.Command(target[0], target[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		log.Fatalf("daemonize: failed to run %q: %v", target[0], err)
	}
}
