//go:build linux

package main

import "syscall"

// dup2 duplicates oldfd onto newfd. Linux (notably arm64) only has dup3.
func dup2(oldfd, newfd int) error {
	return syscall.Dup3(oldfd, newfd, 0)
}
