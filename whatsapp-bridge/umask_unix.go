//go:build !windows

package main

import "syscall"

// restrictNewFiles makes every file this process creates private to the
// current user (the SQLite databases are created by libraries, not by us).
func restrictNewFiles() {
	syscall.Umask(0o077)
}
