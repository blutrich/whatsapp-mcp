package main

// Optional approval dialog: with WHATSAPP_CONFIRM_SENDS=1 the bridge shows a
// desktop dialog with the recipient and the content before anything is sent,
// and sends only if the person at the computer clicks "Send".
//
// This is the one check that does not depend on the AI client: an assistant
// that was manipulated by a message it read cannot click the button.
//
// The text shown comes from the request, so it is never placed inside a
// script or markup. It is passed to the dialog program as a plain argument.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	confirmTimeoutSeconds = 60
	confirmMaxTextRunes   = 600
)

// What the person is asked to approve.
type sendSummary struct {
	recipientName string // saved contact or group name, when known
	recipient     string // the address the message goes to
	text          string
	fileName      string
	fileBytes     int64
}

func confirmSendsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WHATSAPP_CONFIRM_SENDS"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// confirmationBody builds the text of the dialog.
func confirmationBody(s sendSummary) string {
	var b strings.Builder
	b.WriteString("To: ")
	if name := strings.TrimSpace(singleLine(s.recipientName)); name != "" && name != s.recipient {
		b.WriteString(name + "  (" + s.recipient + ")")
	} else {
		b.WriteString(s.recipient)
	}
	if s.fileName != "" {
		b.WriteString(fmt.Sprintf("\n\nFile: %s  (%s)", singleLine(s.fileName), humanSize(s.fileBytes)))
	}
	if text := strings.TrimSpace(s.text); text != "" {
		runes := []rune(removeControlCharacters(text))
		if len(runes) > confirmMaxTextRunes {
			text = string(runes[:confirmMaxTextRunes]) + fmt.Sprintf("…\n\n(%d more characters not shown)", len(runes)-confirmMaxTextRunes)
		} else {
			text = string(runes)
		}
		b.WriteString("\n\nMessage:\n" + text)
	}
	return b.String()
}

func singleLine(text string) string {
	return strings.Join(strings.Fields(removeControlCharacters(text)), " ")
}

// removeControlCharacters keeps line breaks and tabs and drops everything
// else that could change how a dialog or terminal renders the text.
func removeControlCharacters(text string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 32 || (r >= 127 && r < 160) || r == 0x2028 || r == 0x2029:
			return ' '
		// Characters that reorder text on screen could disguise a recipient or message.
		case (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069):
			return -1
		}
		return r
	}, text)
}

func humanSize(bytes int64) string {
	switch {
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(bytes)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", bytes)
}

// The AppleScript is fixed. The dialog text arrives as argument 1, so no
// part of a message can ever be interpreted as script.
const confirmAppleScript = `on run argv
	set answer to display dialog (item 1 of argv) with title "WhatsApp bridge: send this?" buttons {"Don't send", "Send"} default button "Don't send" cancel button "Don't send" with icon caution giving up after ` + "60" + `
	if gave up of answer then return "timeout"
	return button returned of answer
end run`

// confirmationCommand returns the program and arguments that show the dialog,
// or nil when this system has no supported dialog program.
func confirmationCommand(goos, body string, lookPath func(string) (string, error)) []string {
	switch goos {
	case "darwin":
		return []string{"/usr/bin/osascript", "-e", confirmAppleScript, "--", body}
	case "linux":
		if path, err := lookPath("zenity"); err == nil {
			return []string{
				path, "--question", "--no-markup", "--no-wrap",
				"--title=WhatsApp bridge: send this?",
				"--ok-label=Send", "--cancel-label=Don't send", "--default-cancel",
				fmt.Sprintf("--timeout=%d", confirmTimeoutSeconds),
				"--text=" + body,
			}
		}
	}
	return nil
}

var errNoDialog = errors.New("WHATSAPP_CONFIRM_SENDS is on, but this system has no dialog program to ask with (macOS, or Linux with zenity)")

// askToSend shows the dialog and reports whether the person clicked "Send".
// Anything else (the other button, closing the dialog, the timeout, a
// failure to show it) means no.
func askToSend(ctx context.Context, s sendSummary) (bool, error) {
	args := confirmationCommand(runtimeGOOS, confirmationBody(s), exec.LookPath)
	if args == nil {
		return false, errNoDialog
	}
	ctx, cancel := context.WithTimeout(ctx, (confirmTimeoutSeconds+15)*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, args[0], args[1:]...).Output()
	if runtimeGOOS == "darwin" {
		return err == nil && strings.TrimSpace(string(output)) == "Send", nil
	}
	return err == nil, nil // zenity: exit status 0 only for the "Send" button
}

// approveSend is the approval step used by the API; tests replace it.
var approveSend = askToSend
