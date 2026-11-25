package main

// End-to-end tests for the local REST API: a real HTTP server on a real
// loopback port, with a WhatsApp client that is not logged in. No WhatsApp
// account or network access is needed.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"google.golang.org/protobuf/proto"
)

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

type apiFixture struct {
	port  int
	token string
	store *MessageStore
}

func startTestAPI(t *testing.T) apiFixture {
	t.Helper()
	t.Chdir(t.TempDir()) // the bridge keeps its data in ./store
	if err := secureStore(storeDir); err != nil {
		t.Fatal(err)
	}
	container, err := sqlstore.New(context.Background(), "sqlite3", "file:store/whatsapp.db?_foreign_keys=on", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { container.Close() })
	client := whatsmeow.NewClient(container.NewDevice(), nil)

	messageStore, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { messageStore.Close() })

	token, err := loadOrCreateAPIToken(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	server, err := startRESTServer(client, messageStore, port, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	return apiFixture{port: port, token: token, store: messageStore}
}

func (f apiFixture) post(t *testing.T, path string, body any, mutate func(*http.Request)) (int, string) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d%s", f.port, path), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	if mutate != nil {
		mutate(req)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(text)
}

func TestAPIRejectsUnauthenticatedAndBrowserRequests(t *testing.T) {
	api := startTestAPI(t)
	send := map[string]string{"recipient": "972501234567", "message": "hello"}

	cases := []struct {
		name   string
		mutate func(*http.Request)
		status int
	}{
		{"no token", func(r *http.Request) { r.Header.Del("Authorization") }, http.StatusUnauthorized},
		{"wrong token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }, http.StatusUnauthorized},
		{"web page simple request", func(r *http.Request) {
			r.Header.Set("Content-Type", "text/plain")
			r.Header.Set("Origin", "https://evil.example")
			r.Header.Del("Authorization")
		}, http.StatusForbidden},
		{"text/plain with token", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType},
		{"rebinding host", func(r *http.Request) { r.Host = "evil.example" }, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := api.post(t, "/api/send", send, tc.mutate)
			if status != tc.status {
				t.Fatalf("status = %d (%s), want %d", status, strings.TrimSpace(body), tc.status)
			}
		})
	}

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/api/send", api.port))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", resp.StatusCode)
	}
}

func TestAPIAuthenticatedRequests(t *testing.T) {
	api := startTestAPI(t)

	status, body := api.post(t, "/api/health", map[string]string{}, nil)
	if status != http.StatusOK || !strings.Contains(body, `"logged_in":false`) {
		t.Errorf("health = %d %s", status, body)
	}

	// Not logged in: a valid request is refused cleanly, nothing is sent.
	status, body = api.post(t, "/api/send", map[string]string{"recipient": "972501234567", "message": "hello"}, nil)
	if status != http.StatusInternalServerError || !strings.Contains(body, "Not connected to WhatsApp") {
		t.Errorf("send while logged out = %d %s", status, body)
	}

	for name, payload := range map[string]map[string]string{
		"missing recipient": {"message": "x"},
		"missing content":   {"recipient": "972501234567"},
	} {
		if status, _ := api.post(t, "/api/send", payload, nil); status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, status)
		}
	}
	if status, _ := api.post(t, "/api/send", "not an object", nil); status != http.StatusBadRequest {
		t.Errorf("malformed JSON body: status = %d, want 400", status)
	}

	// A request body larger than the limit is refused.
	huge := map[string]string{"recipient": "972501234567", "message": strings.Repeat("a", maxRequestBodyBytes+1024)}
	if status, _ := api.post(t, "/api/send", huge, nil); status != http.StatusBadRequest {
		t.Errorf("oversized body: status = %d, want 400", status)
	}
}

func TestAPIDownloadCannotEscapeStore(t *testing.T) {
	api := startTestAPI(t)
	chat := "972501234567@s.whatsapp.net"
	if err := api.store.StoreChat(chat, "Test", time.Now()); err != nil {
		t.Fatal(err)
	}
	// A document whose sender-chosen file name points outside the store.
	err := api.store.StoreMessage("MSG1", chat, "972501234567", "", time.Now(), false,
		"document", "../../../../escaped.txt", "https://mmg.whatsapp.net/v/t62/x.enc", []byte{1}, []byte{2}, []byte{3}, 10)
	if err != nil {
		t.Fatal(err)
	}

	// Not logged in, so the download itself fails, but no path outside the store may be touched.
	status, body := api.post(t, "/api/download", map[string]string{"message_id": "MSG1", "chat_jid": chat}, nil)
	if status != http.StatusInternalServerError {
		t.Errorf("download while logged out = %d %s", status, body)
	}
	for _, hostile := range []string{"../../etc", "x@evil.example", "nojid", "../store@s.whatsapp.net/../.."} {
		status, body := api.post(t, "/api/download", map[string]string{"message_id": "MSG1", "chat_jid": hostile}, nil)
		if status != http.StatusInternalServerError || strings.Contains(body, `"success":true`) {
			t.Errorf("download with chat %q = %d %s", hostile, status, body)
		}
	}
	cwd, _ := os.Getwd()
	entries, _ := os.ReadDir(filepath.Dir(cwd))
	for _, entry := range entries {
		if entry.Name() == "escaped.txt" {
			t.Fatal("a file was created outside the store")
		}
	}
	matches, _ := filepath.Glob(filepath.Join(cwd, "*"))
	if len(matches) != 1 || filepath.Base(matches[0]) != storeDir {
		t.Errorf("unexpected files next to the store: %v", matches)
	}
}

func TestAPIStoreIsPrivateAndPortConflictIsAnError(t *testing.T) {
	api := startTestAPI(t)
	for _, name := range []string{".", "messages.db", "whatsapp.db", apiTokenFile} {
		info, err := os.Stat(filepath.Join(storeDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("store/%s is readable by other users: %o", name, info.Mode().Perm())
		}
	}
	if _, err := startRESTServer(nil, api.store, api.port, api.token); err == nil {
		t.Error("starting a second API on a busy port did not fail")
	}
}

func TestAPISendRateLimit(t *testing.T) {
	t.Setenv("WHATSAPP_MAX_SENDS_PER_MINUTE", "2")
	api := startTestAPI(t)
	send := map[string]string{"recipient": "972501234567", "message": "hello"}
	for i := 0; i < 2; i++ {
		if status, _ := api.post(t, "/api/send", send, nil); status == http.StatusTooManyRequests {
			t.Fatalf("request %d limited too early", i+1)
		}
	}
	status, body := api.post(t, "/api/send", send, nil)
	if status != http.StatusTooManyRequests || !strings.Contains(body, "Send limit reached") {
		t.Errorf("3rd send = %d %s, want 429", status, body)
	}
}

func TestSentMessagesAreSavedToHistory(t *testing.T) {
	api := startTestAPI(t)
	chat := "972501234567@s.whatsapp.net"
	sentAt := time.Date(2026, 11, 25, 17, 0, 0, 0, time.UTC)

	// A text message to a chat that is not in the database yet.
	text := &waProto.Message{Conversation: proto.String("hello")}
	if err := recordSentMessage(api.store, chat, "Test Contact", "972500000009", "SENT1", sentAt, text, "hello"); err != nil {
		t.Fatal(err)
	}
	// A document with a caption.
	doc := &waProto.Message{DocumentMessage: &waProto.DocumentMessage{
		FileName: proto.String("report.pdf"), URL: proto.String("https://mmg.whatsapp.net/v/t62/x.enc"),
		MediaKey: []byte{1}, FileSHA256: []byte{2}, FileEncSHA256: []byte{3}, FileLength: proto.Uint64(10),
	}}
	if err := recordSentMessage(api.store, chat, "Test Contact", "972500000009", "SENT2", sentAt.Add(time.Minute), doc, "see attached"); err != nil {
		t.Fatal(err)
	}
	// Saving the same message twice must not create a second row.
	if err := recordSentMessage(api.store, chat, "Test Contact", "972500000009", "SENT1", sentAt, text, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := recordSentMessage(api.store, chat, "Test Contact", "972500000009", "", sentAt, text, "hello"); err == nil {
		t.Error("a message without an ID was saved")
	}

	var name string
	if err := api.store.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chat).Scan(&name); err != nil || name != "Test Contact" {
		t.Fatalf("chat row: %q, %v", name, err)
	}
	rows, err := api.store.db.Query("SELECT id, sender, content, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp", chat)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, sender, content, mediaType, filename string
		var fromMe bool
		if err := rows.Scan(&id, &sender, &content, &fromMe, &mediaType, &filename); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s|%s|%s|%v|%s|%s", id, sender, content, fromMe, mediaType, filename))
	}
	want := []string{"SENT1|972500000009|hello|true||", "SENT2|972500000009|see attached|true|document|report.pdf"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("saved messages:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
