package main

// Desktop notifications for the few events a person needs to know about
// while the bridge runs in the background. Best effort: a missing notifier
// is never an error. Set WHATSAPP_NOTIFY=0 to turn them off.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

type notice struct {
	title string
	text  string
}

// Fixed texts only: nothing from WhatsApp is ever put into a notification
// or onto a command line.
var (
	noticePaired    = notice{"WhatsApp linked", "The bridge is connected to your account and ready to use."}
	noticeUnlinked  = notice{"WhatsApp unlinked", "This device was removed. Start the bridge and scan a new QR code."}
	noticeQRTimeout = notice{"WhatsApp pairing timed out", "Nobody scanned the QR code. Start the bridge to try again."}
)

// notificationCommand returns the program and arguments that show a notice
// on the given operating system, or nil when there is no supported notifier.
func notificationCommand(goos string, n notice, lookPath func(string) (string, error)) []string {
	switch goos {
	case "darwin":
		script := fmt.Sprintf("display notification %q with title %q sound name \"Glass\"", n.text, n.title)
		return []string{"/usr/bin/osascript", "-e", script}
	case "linux":
		if path, err := lookPath("notify-send"); err == nil {
			return []string{path, "--app-name=WhatsApp bridge", n.title, n.text}
		}
	}
	return nil
}

func notifyDesktop(n notice, goos string) {
	if os.Getenv("WHATSAPP_NOTIFY") == "0" {
		return
	}
	args := notificationCommand(goos, n, exec.LookPath)
	if args == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, args[0], args[1:]...).Run()
	}()
}
