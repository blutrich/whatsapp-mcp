//go:build windows

package main

// restrictNewFiles is a no-op on Windows, where files inherit the access
// rules of the user's own folders instead of a umask.
func restrictNewFiles() {}
