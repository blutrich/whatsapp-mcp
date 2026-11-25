package main

// Security helpers for the bridge: API authentication, request screening,
// recipient validation, outgoing-file policy and safe local paths for
// downloaded media. Everything here is pure enough to unit test
// (see security_test.go) without a WhatsApp connection.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.mau.fi/whatsmeow/types"
)

const (
	storeDir             = "store"
	apiTokenFile         = "api_token"
	outboxDir            = "outbox"
	defaultAPIPort       = 8080
	maxRequestBodyBytes  = 1 << 20   // 1 MiB of JSON is far more than any request needs
	defaultMaxMediaBytes = 100 << 20 // 100 MiB per outgoing or downloaded file
	maxFilenameRunes     = 120
)

// apiPort returns the port for the local REST API (WHATSAPP_BRIDGE_PORT, default 8080).
func apiPort() (int, error) {
	raw := strings.TrimSpace(os.Getenv("WHATSAPP_BRIDGE_PORT"))
	if raw == "" {
		return defaultAPIPort, nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("WHATSAPP_BRIDGE_PORT must be a port number between 1 and 65535, got %q", raw)
	}
	return port, nil
}

// maxMediaBytes returns the size limit for media files (WHATSAPP_MAX_MEDIA_MB, default 100).
func maxMediaBytes() int64 {
	raw := strings.TrimSpace(os.Getenv("WHATSAPP_MAX_MEDIA_MB"))
	if raw == "" {
		return defaultMaxMediaBytes
	}
	mb, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || mb < 1 || mb > 2048 {
		return defaultMaxMediaBytes
	}
	return mb << 20
}

// mediaRoots returns the optional allowlist of folders outgoing files may come
// from (WHATSAPP_MEDIA_ROOTS, separated like PATH). Empty means "no allowlist".
func mediaRoots() []string {
	var roots []string
	for _, root := range filepath.SplitList(os.Getenv("WHATSAPP_MEDIA_ROOTS")) {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			if abs, err := filepath.Abs(resolved); err == nil {
				roots = append(roots, abs)
			}
		}
	}
	return roots
}

// secureStore makes the store folder private to the current user. The folder
// holds the WhatsApp session keys and the full message history.
func secureStore(dir string) error {
	restrictNewFiles()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return os.Chmod(path, 0o600)
	})
}

// loadOrCreateAPIToken returns the secret every API request must present.
// It is kept in store/api_token (owner-only) so the MCP server, which runs as
// the same user, can read it; web pages and other users cannot.
func loadOrCreateAPIToken(dir string) (string, error) {
	path := filepath.Join(dir, apiTokenFile)
	if data, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(data))
		if len(token) == 64 {
			if _, decodeErr := hex.DecodeString(token); decodeErr == nil {
				if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
					return "", chmodErr
				}
				return token, nil
			}
		}
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return token, nil
}

// apiGuard screens every API request before it reaches a handler:
//   - Host must be this loopback address (blocks DNS rebinding)
//   - no Origin header (browsers always send one on cross-site POSTs)
//   - POST with Content-Type application/json (not a CORS "simple" request,
//     so a web page cannot send it without a preflight we never approve)
//   - Authorization: Bearer <token from store/api_token>
//   - bounded body size
func apiGuard(token string, port int, next http.Handler) http.Handler {
	allowedHosts := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", port): true,
		fmt.Sprintf("localhost:%d", port): true,
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if !allowedHosts[strings.ToLower(r.Host)] {
			http.Error(w, "Forbidden host", http.StatusForbidden)
			return
		}
		if r.Header.Get("Origin") != "" {
			http.Error(w, "Browser requests are not allowed", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
			return
		}
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Missing or wrong API token", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// Servers a message may be sent to, and chats media may be downloaded from.
var sendableServers = map[string]bool{
	types.DefaultUserServer: true, // s.whatsapp.net
	types.GroupServer:       true, // g.us
	types.HiddenUserServer:  true, // lid
}

var knownChatServers = map[string]bool{
	types.DefaultUserServer: true,
	types.GroupServer:       true,
	types.HiddenUserServer:  true,
	types.BroadcastServer:   true,
	types.NewsletterServer:  true,
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseRecipient turns a phone number or JID into a JID we are willing to send to.
func parseRecipient(recipient string) (types.JID, error) {
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return types.JID{}, errors.New("recipient is required")
	}
	if strings.Contains(recipient, "@") {
		jid, err := types.ParseJID(recipient)
		if err != nil {
			return types.JID{}, fmt.Errorf("invalid JID: %v", err)
		}
		if !sendableServers[jid.Server] {
			return types.JID{}, fmt.Errorf("cannot send to %q addresses", jid.Server)
		}
		user := jid.User
		if jid.Server == types.GroupServer {
			// group IDs look like 1203630xxxx or 9725xxxx-1600000000
			user = strings.ReplaceAll(user, "-", "")
		}
		if !isDigits(user) || len(jid.User) > 40 {
			return types.JID{}, errors.New("invalid JID: the part before @ must be numeric")
		}
		return jid.ToNonAD(), nil
	}
	// A phone number: tolerate the usual human formatting, then require digits.
	cleaned := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", ".", "").Replace(recipient)
	cleaned = strings.TrimPrefix(cleaned, "+")
	if !isDigits(cleaned) || len(cleaned) < 5 || len(cleaned) > 20 {
		return types.JID{}, errors.New("recipient must be a phone number with country code (digits only) or a JID")
	}
	return types.NewJID(cleaned, types.DefaultUserServer), nil
}

// parseChatJID validates a chat JID that will be used to look up and store media.
func parseChatJID(chatJID string) (types.JID, error) {
	jid, err := types.ParseJID(strings.TrimSpace(chatJID))
	if err != nil {
		return types.JID{}, fmt.Errorf("invalid chat JID: %v", err)
	}
	if jid.User == "" || !knownChatServers[jid.Server] {
		return types.JID{}, errors.New("invalid chat JID")
	}
	return jid, nil
}

func hasHiddenComponent(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if strings.HasPrefix(part, ".") && part != "." && part != ".." {
			return true
		}
	}
	return false
}

func isWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// validateOutgoingFile decides whether a local file may be uploaded to WhatsApp.
// The bridge is the last line of defence against an agent that was tricked
// into sending something it should not, so the rules are deliberately strict:
// absolute path, a regular file, never anything from the store folder (session
// keys, message history, API token), never hidden files or folders such as
// ~/.ssh or .env, within WHATSAPP_MEDIA_ROOTS when that is set, and not larger
// than the size limit. It returns the fully resolved path to read.
func validateOutgoingFile(path, store string, roots []string, limit int64) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("media path must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", errors.New("media file not found")
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", errors.New("media file not found")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", errors.New("media file not found")
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("media path is not a regular file")
	}
	if info.Size() == 0 {
		return "", errors.New("media file is empty")
	}
	if info.Size() > limit {
		return "", fmt.Errorf("media file is larger than the %d MB limit", limit>>20)
	}
	if storeResolved, err := filepath.EvalSymlinks(store); err == nil {
		if storeAbs, err := filepath.Abs(storeResolved); err == nil && isWithin(storeAbs, resolved) {
			// store/outbox is the one exception: the MCP server puts voice
			// notes it converted with ffmpeg there, and deletes them after.
			if isWithin(filepath.Join(storeAbs, outboxDir), resolved) && filepath.Dir(resolved) == filepath.Join(storeAbs, outboxDir) {
				return resolved, nil
			}
			return "", errors.New("files inside the bridge store cannot be sent")
		}
	}
	if len(roots) > 0 {
		for _, root := range roots {
			if isWithin(root, resolved) {
				return resolved, nil
			}
		}
		return "", errors.New("media file is outside the folders allowed by WHATSAPP_MEDIA_ROOTS")
	}
	if hasHiddenComponent(resolved) {
		return "", errors.New("hidden files and folders cannot be sent (set WHATSAPP_MEDIA_ROOTS to allow a specific folder)")
	}
	return resolved, nil
}

// sanitizeFilename reduces a name chosen by a remote sender to something safe
// to use as a single path component.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	}
	var b strings.Builder
	count := 0
	for _, r := range name {
		if count >= maxFilenameRunes {
			break
		}
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), unicode.IsMark(r):
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_' || r == ' ' || r == '(' || r == ')' || r == '@':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		count++
	}
	cleaned := strings.TrimLeft(strings.TrimSpace(b.String()), ". ")
	cleaned = strings.TrimRight(cleaned, ". ")
	if cleaned == "" {
		return "file"
	}
	return cleaned
}

// mediaLocalPath returns where a downloaded attachment is saved:
// store/<chat>/<message id>_<file name>, always inside the store. The message
// ID keeps two attachments with the same name from overwriting each other.
func mediaLocalPath(store string, chat types.JID, messageID, filename string) (dir string, path string, err error) {
	storeAbs, err := filepath.Abs(store)
	if err != nil {
		return "", "", err
	}
	dir = filepath.Join(storeAbs, sanitizeFilename(chat.String()))
	path = filepath.Join(dir, sanitizeFilename(messageID)+"_"+sanitizeFilename(filename))
	if !isWithin(storeAbs, path) || filepath.Dir(path) != dir || dir == storeAbs {
		return "", "", errors.New("refusing to write outside the store folder")
	}
	return dir, path, nil
}

// mimeTypeForDocument picks a MIME type for a document from its extension.
func mimeTypeForDocument(path string) string {
	if detected := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); detected != "" {
		return detected
	}
	return "application/octet-stream"
}

// parseAllowedRecipients reads WHATSAPP_ALLOWED_RECIPIENTS: an optional list
// of phone numbers or JIDs (comma or whitespace separated) that messages may
// be sent to. Empty means no restriction. A bad entry is an error, so a typo
// never silently turns the restriction off.
func parseAllowedRecipients(raw string) ([]types.JID, error) {
	var allowed []types.JID
	for _, entry := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || unicode.IsSpace(r) }) {
		jid, err := parseRecipient(entry)
		if err != nil {
			return nil, fmt.Errorf("WHATSAPP_ALLOWED_RECIPIENTS: %q: %v", entry, err)
		}
		allowed = append(allowed, jid)
	}
	return allowed, nil
}

// recipientAllowed reports whether jid may be sent to under the allowlist.
func recipientAllowed(jid types.JID, allowed []types.JID) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if candidate.User == jid.User && candidate.Server == jid.Server {
			return true
		}
	}
	return false
}

// sendLimiter caps how many messages may be sent per minute, so a confused
// or manipulated agent cannot flood contacts (or get the account banned).
type sendLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	sent   []time.Time
}

func newSendLimiter() *sendLimiter {
	limit := 30
	if raw := strings.TrimSpace(os.Getenv("WHATSAPP_MAX_SENDS_PER_MINUTE")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 1 && parsed <= 1000 {
			limit = parsed
		}
	}
	return &sendLimiter{limit: limit, window: time.Minute}
}

// allow records a send attempt and reports whether it is within the limit.
func (l *sendLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.sent[:0]
	for _, at := range l.sent {
		if now.Sub(at) < l.window {
			kept = append(kept, at)
		}
	}
	l.sent = kept
	if len(l.sent) >= l.limit {
		return false
	}
	l.sent = append(l.sent, now)
	return true
}

// allowedRecipients is set once at startup from WHATSAPP_ALLOWED_RECIPIENTS.
var allowedRecipients []types.JID

// Extensions for the media types WhatsApp sends without a file name.
var mediaExtensions = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif",
	"video/mp4": ".mp4", "video/quicktime": ".mov", "video/3gpp": ".3gp", "video/webm": ".webm",
	"audio/ogg": ".ogg", "audio/mpeg": ".mp3", "audio/mp4": ".m4a", "audio/aac": ".aac", "audio/wav": ".wav",
}

// mediaExtension returns the file extension for a MIME type reported by a
// sender, or fallback when it is unknown. Only values from the table above
// are ever returned, so a sender cannot choose an arbitrary extension.
func mediaExtension(mimeType, fallback string) string {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
	if ext, ok := mediaExtensions[base]; ok {
		return ext
	}
	return fallback
}
