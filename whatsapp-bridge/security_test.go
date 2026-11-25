package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func guardedRequest(t *testing.T, mutate func(*http.Request)) (int, bool) {
	t.Helper()
	reached := false
	handler := apiGuard(testToken, 8080, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/send", strings.NewReader(`{"recipient":"1","message":"x"}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, reached
}

func TestAPIGuard(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*http.Request)
		status int
	}{
		{"valid request", nil, http.StatusOK},
		{"localhost host", func(r *http.Request) { r.Host = "localhost:8080" }, http.StatusOK},
		{"json with charset", func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=utf-8") }, http.StatusOK},
		{"no token", func(r *http.Request) { r.Header.Del("Authorization") }, http.StatusUnauthorized},
		{"wrong token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+strings.Repeat("f", 64)) }, http.StatusUnauthorized},
		{"token without scheme", func(r *http.Request) { r.Header.Set("Authorization", testToken) }, http.StatusUnauthorized},
		{"web page form post (text/plain)", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType},
		{"no content type", func(r *http.Request) { r.Header.Del("Content-Type") }, http.StatusUnsupportedMediaType},
		{"cross-site origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, http.StatusForbidden},
		{"same-site origin still refused", func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:8080") }, http.StatusForbidden},
		{"dns rebinding host", func(r *http.Request) { r.Host = "evil.example:8080" }, http.StatusForbidden},
		{"wrong port in host", func(r *http.Request) { r.Host = "127.0.0.1:9999" }, http.StatusForbidden},
		{"GET", func(r *http.Request) { r.Method = http.MethodGet }, http.StatusMethodNotAllowed},
		{"OPTIONS preflight", func(r *http.Request) { r.Method = http.MethodOptions }, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, reached := guardedRequest(t, tc.mutate)
			if status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
			if reached != (tc.status == http.StatusOK) {
				t.Fatalf("handler reached = %v with status %d", reached, status)
			}
		})
	}
}

func TestAPIGuardLimitsBody(t *testing.T) {
	handler := apiGuard(testToken, 8080, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, maxRequestBodyBytes+10)
		total := 0
		for {
			n, err := r.Body.Read(buf)
			total += n
			if err != nil {
				break
			}
		}
		if total > maxRequestBodyBytes {
			t.Errorf("read %d bytes, limit is %d", total, maxRequestBodyBytes)
		}
	}))
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/send", strings.NewReader(strings.Repeat("a", maxRequestBodyBytes*2)))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testToken)
	handler.ServeHTTP(httptest.NewRecorder(), req)
}

func TestParseRecipient(t *testing.T) {
	good := map[string]string{
		"972501234567":                   "972501234567@s.whatsapp.net",
		"+972 50-123-4567":               "972501234567@s.whatsapp.net",
		"972501234567@s.whatsapp.net":    "972501234567@s.whatsapp.net",
		"120363041234567890@g.us":        "120363041234567890@g.us",
		"972501234567-1600000000@g.us":   "972501234567-1600000000@g.us",
		"123456789012345@lid":            "123456789012345@lid",
		"972501234567:12@s.whatsapp.net": "972501234567@s.whatsapp.net",
	}
	for in, want := range good {
		jid, err := parseRecipient(in)
		if err != nil {
			t.Errorf("parseRecipient(%q) error: %v", in, err)
			continue
		}
		if jid.String() != want {
			t.Errorf("parseRecipient(%q) = %q, want %q", in, jid.String(), want)
		}
	}
	bad := []string{
		"", "   ", "abc", "12", "status", "status@broadcast", "123@newsletter",
		"972501234567@evil.example", "@s.whatsapp.net", "../../etc@s.whatsapp.net",
		"9725012345678901234567890", "97250\n1234567", "name@g.us",
	}
	for _, in := range bad {
		if jid, err := parseRecipient(in); err == nil {
			t.Errorf("parseRecipient(%q) accepted as %q, want error", in, jid.String())
		}
	}
}

func TestParseChatJID(t *testing.T) {
	for _, in := range []string{"972501234567@s.whatsapp.net", "120363041234567890@g.us", "123@lid", "status@broadcast", "123@newsletter"} {
		if _, err := parseChatJID(in); err != nil {
			t.Errorf("parseChatJID(%q) error: %v", in, err)
		}
	}
	for _, in := range []string{"", "nojid", "x@evil.example", "@g.us"} {
		if _, err := parseChatJID(in); err == nil {
			t.Errorf("parseChatJID(%q) accepted, want error", in)
		}
	}
}

func writeFile(t *testing.T, path string, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateOutgoingFile(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(base, "bridge", "store")
	limit := int64(1024)

	photo := writeFile(t, filepath.Join(base, "docs", "photo.jpg"), "jpeg")
	sessionDB := writeFile(t, filepath.Join(store, "whatsapp.db"), "keys")
	token := writeFile(t, filepath.Join(store, "api_token"), "secret")
	received := writeFile(t, filepath.Join(store, "1@s.whatsapp.net", "a_b.pdf"), "pdf")
	voice := writeFile(t, filepath.Join(store, outboxDir, "voice.ogg"), "OggS")
	nested := writeFile(t, filepath.Join(store, outboxDir, "deep", "voice.ogg"), "OggS")
	sshKey := writeFile(t, filepath.Join(base, ".ssh", "id_ed25519"), "key")
	dotenv := writeFile(t, filepath.Join(base, "project", ".env"), "SECRET=1")
	empty := writeFile(t, filepath.Join(base, "docs", "empty.txt"), "")
	big := writeFile(t, filepath.Join(base, "docs", "big.bin"), strings.Repeat("x", 2048))
	link := filepath.Join(base, "docs", "innocent.jpg")
	if err := os.Symlink(sessionDB, link); err != nil {
		t.Fatal(err)
	}
	hiddenLink := filepath.Join(base, "docs", "notes.txt")
	if err := os.Symlink(sshKey, hiddenLink); err != nil {
		t.Fatal(err)
	}

	allowed := []string{photo, voice}
	for _, path := range allowed {
		if _, err := validateOutgoingFile(path, store, nil, limit); err != nil {
			t.Errorf("validateOutgoingFile(%q) rejected: %v", path, err)
		}
	}
	refused := map[string]string{
		"relative path":           "docs/photo.jpg",
		"missing file":            filepath.Join(base, "docs", "nope.jpg"),
		"directory":               filepath.Join(base, "docs"),
		"session database":        sessionDB,
		"api token":               token,
		"received media in store": received,
		"nested outbox file":      nested,
		"symlink into store":      link,
		"symlink to hidden file":  hiddenLink,
		"ssh key":                 sshKey,
		"dotenv":                  dotenv,
		"empty file":              empty,
		"over size limit":         big,
		"traversal into store":    filepath.Join(base, "docs", "..", "bridge", "store", "whatsapp.db"),
		"character device":        "/dev/zero",
	}
	for name, path := range refused {
		if got, err := validateOutgoingFile(path, store, nil, limit); err == nil {
			t.Errorf("%s: validateOutgoingFile(%q) allowed as %q, want refusal", name, path, got)
		}
	}

	// With an allowlist, only files under it pass (plus the outbox).
	roots := []string{filepath.Join(base, "docs")}
	if _, err := validateOutgoingFile(photo, store, roots, limit); err != nil {
		t.Errorf("file inside allowlisted folder rejected: %v", err)
	}
	outside := writeFile(t, filepath.Join(base, "other", "file.txt"), "x")
	if _, err := validateOutgoingFile(outside, store, roots, limit); err == nil {
		t.Error("file outside the allowlist was allowed")
	}
	if _, err := validateOutgoingFile(link, store, roots, limit); err == nil {
		t.Error("symlink from allowlisted folder into the store was allowed")
	}
	if _, err := validateOutgoingFile(voice, store, roots, limit); err != nil {
		t.Errorf("outbox file rejected with allowlist: %v", err)
	}
	// An allowlisted hidden folder is the documented way to permit one.
	hiddenRoot := []string{filepath.Join(base, "project")}
	if _, err := validateOutgoingFile(dotenv, store, hiddenRoot, limit); err != nil {
		t.Errorf("explicitly allowlisted folder rejected: %v", err)
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"report.pdf":                  "report.pdf",
		"../../../../etc/passwd":      "passwd",
		"..\\..\\windows\\evil.exe":   "evil.exe",
		"/abs/path/file.txt":          "file.txt",
		".bashrc":                     "bashrc",
		"...":                         "file",
		"":                            "file",
		"a\x00b\nc.txt":               "a_b_c.txt",
		"חשבונית מס.pdf":              "חשבונית מס.pdf",
		"name:with*bad?chars.txt":     "name_with_bad_chars.txt",
		"972501234567@s.whatsapp.net": "972501234567@s.whatsapp.net",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
	long := sanitizeFilename(strings.Repeat("a", 5000) + ".pdf")
	if len([]rune(long)) > maxFilenameRunes {
		t.Errorf("long name not truncated: %d runes", len([]rune(long)))
	}
}

func TestMediaLocalPathStaysInStore(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	storeAbs, _ := filepath.Abs(store)
	chat := types.NewJID("972501234567", types.DefaultUserServer)
	hostile := []string{
		"../../../../escaped.txt", "/etc/passwd", "..", ".", "", "a/../../b", "..\\..\\x", "\x00", "whatsapp.db",
	}
	for _, id := range append(hostile, "3EB0ABCDEF") {
		for _, name := range hostile {
			dir, path, err := mediaLocalPath(store, chat, id, name)
			if err != nil {
				continue // refusing is fine
			}
			if !isWithin(storeAbs, path) || filepath.Dir(path) != dir || filepath.Dir(dir) != storeAbs {
				t.Errorf("mediaLocalPath(id=%q, name=%q) = %q escapes %q", id, name, path, storeAbs)
			}
			if filepath.Dir(path) == storeAbs {
				t.Errorf("mediaLocalPath(id=%q, name=%q) writes directly into the store root", id, name)
			}
		}
	}
	// Two attachments with the same generated name no longer share a file.
	_, first, _ := mediaLocalPath(store, chat, "MSG1", "image_20260101_120000.jpg")
	_, second, _ := mediaLocalPath(store, chat, "MSG2", "image_20260101_120000.jpg")
	if first == second {
		t.Error("different messages map to the same local file")
	}
}

func TestAPITokenAndStorePermissions(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(filepath.Join(store, "chat@s.whatsapp.net"), 0o755); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(store, "messages.db")
	if err := os.WriteFile(loose, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := secureStore(store); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{store: 0o700, loose: 0o600, filepath.Join(store, "chat@s.whatsapp.net"): 0o700} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode = %o, want %o", path, info.Mode().Perm(), want)
		}
	}

	first, err := loadOrCreateAPIToken(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Fatalf("token length = %d, want 64", len(first))
	}
	again, err := loadOrCreateAPIToken(store)
	if err != nil || again != first {
		t.Fatalf("token not stable across restarts: %q vs %q (%v)", first, again, err)
	}
	info, _ := os.Stat(filepath.Join(store, apiTokenFile))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %o, want 600", info.Mode().Perm())
	}
	// A damaged token file is replaced, not trusted.
	os.WriteFile(filepath.Join(store, apiTokenFile), []byte("short"), 0o644)
	replaced, err := loadOrCreateAPIToken(store)
	if err != nil || len(replaced) != 64 || replaced == "short" {
		t.Fatalf("damaged token not replaced: %q (%v)", replaced, err)
	}
}

func TestAPIPort(t *testing.T) {
	t.Setenv("WHATSAPP_BRIDGE_PORT", "")
	if port, err := apiPort(); err != nil || port != defaultAPIPort {
		t.Errorf("default port = %d, %v", port, err)
	}
	t.Setenv("WHATSAPP_BRIDGE_PORT", "8123")
	if port, err := apiPort(); err != nil || port != 8123 {
		t.Errorf("custom port = %d, %v", port, err)
	}
	for _, bad := range []string{"0", "70000", "abc", "80; rm -rf /"} {
		t.Setenv("WHATSAPP_BRIDGE_PORT", bad)
		if _, err := apiPort(); err == nil {
			t.Errorf("port %q accepted", bad)
		}
	}
}

func TestSenderUser(t *testing.T) {
	cases := map[string]string{
		"972501234567@s.whatsapp.net":    "972501234567",
		"972501234567:12@s.whatsapp.net": "972501234567",
		"123456789012345@lid":            "123456789012345",
		"972501234567":                   "972501234567",
	}
	for in, want := range cases {
		if got := senderUser(in); got != want {
			t.Errorf("senderUser(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAllowedRecipients(t *testing.T) {
	none, err := parseAllowedRecipients("  ")
	if err != nil || len(none) != 0 {
		t.Fatalf("empty allowlist = %v, %v", none, err)
	}
	anyone, _ := parseRecipient("15550001111")
	if !recipientAllowed(anyone, none) {
		t.Error("no allowlist must allow everyone")
	}

	allowed, err := parseAllowedRecipients("+972 501234567, 120363041234567890@g.us;15551230000")
	if err == nil {
		t.Fatalf("a space inside a number splits it into entries that must be rejected: %v", allowed)
	}
	allowed, err = parseAllowedRecipients("+972501234567, 120363041234567890@g.us;15551230000")
	if err != nil || len(allowed) != 3 {
		t.Fatalf("allowlist = %v, %v", allowed, err)
	}
	for in, want := range map[string]bool{
		"972501234567":                true,
		"972501234567@s.whatsapp.net": true,
		"120363041234567890@g.us":     true,
		"15551230000":                 true,
		"15550001111":                 false,
		"972501234567@lid":            false,
		"120363041234567891@g.us":     false,
	} {
		jid, err := parseRecipient(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := recipientAllowed(jid, allowed); got != want {
			t.Errorf("recipientAllowed(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := parseAllowedRecipients("972501234567, not-a-number"); err == nil {
		t.Error("a bad allowlist entry must be an error, not ignored")
	}
}

func TestSendLimiter(t *testing.T) {
	limiter := &sendLimiter{limit: 3, window: time.Minute}
	start := time.Now()
	for i := 0; i < 3; i++ {
		if !limiter.allow(start.Add(time.Duration(i) * time.Second)) {
			t.Fatalf("send %d refused within the limit", i+1)
		}
	}
	if limiter.allow(start.Add(10 * time.Second)) {
		t.Error("4th send within a minute was allowed")
	}
	if !limiter.allow(start.Add(61 * time.Second)) {
		t.Error("send refused after the window passed")
	}
}

func TestDirectPathKeepsAccessSignature(t *testing.T) {
	cases := map[string]string{
		"https://mmg.whatsapp.net/v/t62.7118-24/1_2_n.enc?ccb=11-4&oh=01_AB&oe=6A&_nc_sid=5e03e0&mms3=true": "/v/t62.7118-24/1_2_n.enc?ccb=11-4&oh=01_AB&oe=6A&_nc_sid=5e03e0&mms3=true",
		"https://mmg.whatsapp.net/o1/v/t24/f2/m233/AQ.enc?ccb=9-4&oh=01_X&oe=6B":                            "/o1/v/t24/f2/m233/AQ.enc?ccb=9-4&oh=01_X&oe=6B",
		"https://media-tlv1-1.cdn.whatsapp.net/v/t62/1.enc?x=1":                                             "/v/t62/1.enc?x=1",
		"https://mmg.whatsapp.net/v/t62/no-query.enc":                                                       "/v/t62/no-query.enc?",
		"not a url": "not a url",
	}
	for in, want := range cases {
		if got := extractDirectPathFromURL(in); got != want {
			t.Errorf("extractDirectPathFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWaitUntil(t *testing.T) {
	// Becomes ready after a short while, as login does after pairing.
	start := time.Now()
	if !waitUntil(5*time.Second, func() bool { return time.Since(start) > 300*time.Millisecond }) {
		t.Error("waitUntil gave up before the condition became true")
	}
	if waitUntil(300*time.Millisecond, func() bool { return false }) {
		t.Error("waitUntil reported success for a condition that never became true")
	}
}

func TestNotificationCommand(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/notify-send", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }

	mac := notificationCommand("darwin", noticePaired, missing)
	if len(mac) != 3 || mac[0] != "/usr/bin/osascript" || !strings.Contains(mac[2], `with title "WhatsApp linked"`) {
		t.Errorf("macOS command = %q", mac)
	}
	linux := notificationCommand("linux", noticeUnlinked, found)
	if len(linux) != 4 || linux[2] != "WhatsApp unlinked" {
		t.Errorf("Linux command = %q", linux)
	}
	if got := notificationCommand("linux", noticePaired, missing); got != nil {
		t.Errorf("no notifier installed, got %q", got)
	}
	if got := notificationCommand("windows", noticePaired, found); got != nil {
		t.Errorf("unsupported system, got %q", got)
	}
	// Notification texts are fixed and must stay free of quoting hazards.
	for _, n := range []notice{noticePaired, noticeUnlinked, noticeQRTimeout} {
		if strings.ContainsAny(n.title+n.text, "\"\\`$\n") {
			t.Errorf("notice %q contains a character that needs escaping", n.title)
		}
	}
	t.Setenv("WHATSAPP_NOTIFY", "0")
	notifyDesktop(noticePaired, "darwin") // must be a no-op, not a popup during tests
}
