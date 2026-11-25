package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestConfirmationBody(t *testing.T) {
	body := confirmationBody(sendSummary{recipientName: "Coach", recipient: "972500000077@s.whatsapp.net", text: "trained today"})
	for _, want := range []string{"To: Coach  (972500000077@s.whatsapp.net)", "Message:\ntrained today"} {
		if !strings.Contains(body, want) {
			t.Errorf("body misses %q:\n%s", want, body)
		}
	}
	// No saved name: the address alone, not repeated.
	if body := confirmationBody(sendSummary{recipient: "15550001111@s.whatsapp.net", text: "x"}); !strings.HasPrefix(body, "To: 15550001111@s.whatsapp.net\n") {
		t.Errorf("unexpected body: %q", body)
	}
	file := confirmationBody(sendSummary{recipient: "1@g.us", fileName: "report.pdf", fileBytes: 3 << 20})
	if !strings.Contains(file, "File: report.pdf  (3.0 MB)") || strings.Contains(file, "Message:") {
		t.Errorf("file body: %q", file)
	}
	// A long message is cut, and says so.
	long := confirmationBody(sendSummary{recipient: "1@g.us", text: strings.Repeat("a", 5000)})
	if !strings.Contains(long, "(4400 more characters not shown)") || len([]rune(long)) > 800 {
		t.Errorf("long text not truncated properly (%d runes)", len([]rune(long)))
	}
	// A contact name cannot add lines, hide text with control characters,
	// or reverse the displayed text to disguise who the recipient is.
	hostile := confirmationBody(sendSummary{
		recipientName: "Mom\nTo: someone else\x1b[2J\u202egro.live",
		recipient:     "15550001111@s.whatsapp.net",
		text:          "hi\u202e",
	})
	firstLine := strings.SplitN(hostile, "\n", 2)[0]
	if !strings.Contains(firstLine, "Mom To: someone else") || !strings.HasSuffix(firstLine, "(15550001111@s.whatsapp.net)") {
		t.Errorf("recipient line can be forged: %q", firstLine)
	}
	if strings.ContainsAny(hostile, "\x1b\u202e") {
		t.Error("control or text-reversing characters reached the dialog")
	}
}

func TestConfirmationCommandNeverPutsTextInAScript(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/zenity", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	payload := `" & (do shell script "touch /tmp/pwned") & "` + "\n<b>markup</b> $(id) `id`"

	mac := confirmationCommand("darwin", payload, missing)
	if len(mac) != 5 || mac[0] != "/usr/bin/osascript" || mac[2] != confirmAppleScript || mac[3] != "--" || mac[4] != payload {
		t.Fatalf("macOS command = %q", mac)
	}
	if strings.Contains(mac[2], "pwned") || !strings.Contains(mac[2], "item 1 of argv") {
		t.Error("the script must be fixed and read the text from its arguments")
	}
	if !strings.Contains(confirmAppleScript, `default button "Don't send"`) || !strings.Contains(confirmAppleScript, "giving up after 60") {
		t.Error("the dialog must default to not sending and time out")
	}

	linux := confirmationCommand("linux", payload, found)
	joined := strings.Join(linux, " ")
	if linux == nil || !strings.Contains(joined, "--no-markup") || !strings.Contains(joined, "--default-cancel") || linux[len(linux)-1] != "--text="+payload {
		t.Errorf("Linux command = %q", linux)
	}
	if confirmationCommand("linux", payload, missing) != nil || confirmationCommand("windows", payload, found) != nil {
		t.Error("a system without a dialog program must report none")
	}
}

func TestAPIApprovalDialog(t *testing.T) {
	api := startTestAPI(t)
	send := map[string]string{"recipient": "972501234567", "message": "hello"}
	var asked []sendSummary
	original := approveSend
	t.Cleanup(func() { approveSend = original })

	// Off by default: nobody is asked.
	approveSend = func(ctx context.Context, s sendSummary) (bool, error) { asked = append(asked, s); return false, nil }
	if status, body := api.post(t, "/api/send", send, nil); status != http.StatusInternalServerError || !strings.Contains(body, "Not connected") || len(asked) != 0 {
		t.Fatalf("with the dialog off: %d %s, asked %d times", status, body, len(asked))
	}

	t.Setenv("WHATSAPP_CONFIRM_SENDS", "1")

	// Declined: refused before any attempt to send.
	status, body := api.post(t, "/api/send", send, nil)
	if status != http.StatusForbidden || !strings.Contains(body, "did not approve") {
		t.Errorf("declined send = %d %s", status, body)
	}
	if len(asked) != 1 || asked[0].recipient != "972501234567@s.whatsapp.net" || asked[0].text != "hello" {
		t.Errorf("dialog was shown %d times with %+v", len(asked), asked)
	}

	// Approved: the send goes ahead (and fails here only because the test client is not logged in).
	approveSend = func(ctx context.Context, s sendSummary) (bool, error) { return true, nil }
	if status, body := api.post(t, "/api/send", send, nil); status != http.StatusInternalServerError || !strings.Contains(body, "Not connected") {
		t.Errorf("approved send = %d %s", status, body)
	}

	// No way to ask: fail closed.
	approveSend = func(ctx context.Context, s sendSummary) (bool, error) { return false, errNoDialog }
	if status, body := api.post(t, "/api/send", send, nil); status != http.StatusForbidden || !strings.Contains(body, "no dialog program") {
		t.Errorf("send without a dialog program = %d %s", status, body)
	}

	// A request the bridge would refuse anyway never reaches the dialog.
	asked = nil
	approveSend = func(ctx context.Context, s sendSummary) (bool, error) { asked = append(asked, s); return true, nil }
	for _, bad := range []map[string]string{
		{"recipient": "not a number", "message": "x"},
		{"recipient": "972501234567", "media_path": "/etc/../etc/hosts-nope"},
	} {
		if status, _ := api.post(t, "/api/send", bad, nil); status != http.StatusBadRequest {
			t.Errorf("invalid request %v = %d, want 400", bad, status)
		}
	}
	if len(asked) != 0 {
		t.Errorf("dialog shown for invalid requests: %+v", asked)
	}
}

func TestStatusReportHasNoSecrets(t *testing.T) {
	api := startTestAPI(t)
	t.Setenv("WHATSAPP_CONFIRM_SENDS", "1")
	status, body := api.post(t, "/api/health", map[string]string{}, nil)
	if status != http.StatusOK {
		t.Fatalf("health = %d %s", status, body)
	}
	for _, want := range []string{`"connected":false`, `"logged_in":false`, `"approval_dialog_before_sending":true`, `"send_limit_per_minute":30`, `"recipients_restricted":false`} {
		if !strings.Contains(body, want) {
			t.Errorf("status misses %s: %s", want, body)
		}
	}
	if strings.Contains(body, api.token) || strings.Contains(body, "store") || strings.Contains(body, "@") {
		t.Errorf("status leaks a secret, a path or an address: %s", body)
	}
}
