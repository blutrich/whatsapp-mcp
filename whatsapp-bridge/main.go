package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"
	"rsc.io/qr"

	"bytes"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Message represents a chat message for our client
type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

// Database handler for storing message history
type MessageStore struct {
	db *sql.DB
}

// Initialize message store
func NewMessageStore() (*MessageStore, error) {
	// Create directory for database if it doesn't exist (private to this user)
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}

	// Open SQLite database for messages
	db, err := sql.Open("sqlite3", "file:store/messages.db?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	// Create tables if they don't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);
		
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);

		CREATE INDEX IF NOT EXISTS idx_messages_chat_time ON messages(chat_jid, timestamp);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	return &MessageStore{db: db}, nil
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// Store a chat in the database
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		"INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
		jid, name, lastMessageTime,
	)
	return err
}

// Store a message in the database
func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	// Only store if there's actual content or media
	if content == "" && mediaType == "" {
		return nil
	}

	_, err := store.db.Exec(
		`INSERT OR REPLACE INTO messages 
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length) 
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength,
	)
	return err
}

// Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe, &msg.MediaType, &msg.Filename)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp
		messages = append(messages, msg)
	}

	return messages, nil
}

// Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var lastMessageTime time.Time
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		chats[jid] = lastMessageTime
	}

	return chats, nil
}

// Extract text content from a message
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}

	// Try to get text content
	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	}

	// For now, we're ignoring non-text messages
	return ""
}

// SendMessageResponse represents the response for the send message API
type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
}

// Function to send a WhatsApp message
func sendWhatsAppMessage(client *whatsmeow.Client, messageStore *MessageStore, recipient string, message string, mediaPath string) (bool, string) {
	if !client.IsConnected() || !client.IsLoggedIn() {
		return false, "Not connected to WhatsApp"
	}

	// Only phone numbers and user/group JIDs are accepted
	recipientJID, err := parseRecipient(recipient)
	if err != nil {
		return false, err.Error()
	}
	if !recipientAllowed(recipientJID, allowedRecipients) {
		return false, "This recipient is not in WHATSAPP_ALLOWED_RECIPIENTS, so the bridge will not send to it"
	}

	msg := &waProto.Message{}

	// Check if we have media to send
	if mediaPath != "" {
		// The file must pass the outgoing-file policy before it is read
		resolvedPath, err := validateOutgoingFile(mediaPath, storeDir, mediaRoots(), maxMediaBytes())
		if err != nil {
			return false, fmt.Sprintf("Cannot send file: %v", err)
		}

		// Read media file
		mediaData, err := os.ReadFile(resolvedPath)
		if err != nil {
			return false, "Error reading media file"
		}

		// Determine media type and mime type based on file extension
		fileExt := strings.ToLower(strings.TrimPrefix(filepath.Ext(resolvedPath), "."))
		var mediaType whatsmeow.MediaType
		var mimeType string

		// Handle different media types
		switch fileExt {
		// Image types
		case "jpg", "jpeg":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/jpeg"
		case "png":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/png"
		case "gif":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/gif"
		case "webp":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/webp"

		// Audio types
		case "ogg":
			mediaType = whatsmeow.MediaAudio
			mimeType = "audio/ogg; codecs=opus"

		// Video types
		case "mp4":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/mp4"
		case "avi":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/avi"
		case "mov":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/quicktime"

		// Document types (for any other file type)
		default:
			mediaType = whatsmeow.MediaDocument
			mimeType = mimeTypeForDocument(resolvedPath)
		}

		// Upload media to WhatsApp servers
		uploadCtx, cancelUpload := context.WithTimeout(context.Background(), 5*time.Minute)
		resp, err := client.Upload(uploadCtx, mediaData, mediaType)
		cancelUpload()
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err)
		}

		// Create the appropriate message type based on media type
		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waProto.ImageMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			// Handle ogg audio files
			var seconds uint32 = 30 // Default fallback
			var waveform []byte = nil

			// Try to analyze the ogg file
			if strings.Contains(mimeType, "ogg") {
				analyzedSeconds, analyzedWaveform, err := analyzeOggOpus(mediaData)
				if err == nil {
					seconds = analyzedSeconds
					waveform = analyzedWaveform
				} else {
					return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err)
				}
			} else {
				fmt.Printf("Not an Ogg Opus file: %s\n", mimeType)
			}

			msg.AudioMessage = &waProto.AudioMessage{
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
				Seconds:       proto.Uint32(seconds),
				PTT:           proto.Bool(true),
				Waveform:      waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waProto.VideoMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			msg.DocumentMessage = &waProto.DocumentMessage{
				Title:         proto.String(filepath.Base(mediaPath)),
				FileName:      proto.String(filepath.Base(mediaPath)),
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		}
	} else {
		msg.Conversation = proto.String(message)
	}

	// Send message
	sendCtx, cancelSend := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelSend()
	resp, err := client.SendMessage(sendCtx, recipientJID, msg)

	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err)
	}

	// WhatsApp does not echo our own sends back to this device, so the
	// message is saved here; otherwise it would be missing from the history.
	if messageStore != nil {
		ownUser := ""
		if ownID := client.Store.ID; ownID != nil {
			ownUser = ownID.User
		}
		chatJID := recipientJID.String()
		chatName := GetChatName(client, messageStore, recipientJID, chatJID, nil, "", nil)
		if err := recordSentMessage(messageStore, chatJID, chatName, ownUser, string(resp.ID), resp.Timestamp, msg, message); err != nil {
			fmt.Printf("Message sent, but it could not be saved to the local history: %v\n", err)
		}
	}

	return true, fmt.Sprintf("Message sent to %s", recipientJID.String())
}

// recordSentMessage saves a message this bridge just sent, so it appears in
// the local history next to the replies it gets.
func recordSentMessage(messageStore *MessageStore, chatJID, chatName, ownUser, messageID string, sentAt time.Time, msg *waProto.Message, text string) error {
	if messageID == "" {
		return errors.New("the sent message has no ID")
	}
	if sentAt.IsZero() {
		sentAt = time.Now()
	}
	if err := messageStore.StoreChat(chatJID, chatName, sentAt); err != nil {
		return err
	}
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg)
	return messageStore.StoreMessage(messageID, chatJID, ownUser, text, sentAt, true,
		mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength)
}

// Extract media info from a message
// Image, video and audio messages carry no file name, so one is generated here.
// The message ID is added at download time, which keeps saved files distinct.
func extractMediaInfo(msg *waProto.Message) (mediaType string, filename string, url string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", nil, nil, nil, 0
	}

	// Check for image message
	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + time.Now().Format("20060102_150405") + mediaExtension(img.GetMimetype(), ".jpg"),
			img.GetURL(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}

	// Check for video message
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + time.Now().Format("20060102_150405") + mediaExtension(vid.GetMimetype(), ".mp4"),
			vid.GetURL(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}

	// Check for audio message
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + time.Now().Format("20060102_150405") + mediaExtension(aud.GetMimetype(), ".ogg"),
			aud.GetURL(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}

	// Check for document message
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = "document_" + time.Now().Format("20060102_150405")
		}
		return "document", filename,
			doc.GetURL(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	return "", "", "", nil, nil, nil, 0
}

// Handle regular incoming messages with media support
func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	// Save message to database
	chatJID := msg.Info.Chat.String()
	sender := msg.Info.Sender.User

	// Get appropriate chat name (pass nil for conversation since we don't have one for regular messages)
	name := GetChatName(client, messageStore, msg.Info.Chat, chatJID, nil, sender, logger)

	// Update chat in database with the message timestamp (keeps last message time updated)
	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	// Extract text content
	content := extractTextContent(msg.Message)

	// Extract media info
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message)

	// Skip if there's no content and no media
	if content == "" && mediaType == "" {
		return
	}

	// Store message in database
	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		content,
		msg.Info.Timestamp,
		msg.Info.IsFromMe,
		mediaType,
		filename,
		url,
		mediaKey,
		fileSHA256,
		fileEncSHA256,
		fileLength,
	)

	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
	} else {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		// Message text stays out of the log unless WHATSAPP_LOG_MESSAGES=1
		if logMessageBodies() {
			if mediaType != "" {
				fmt.Printf("[%s] %s %s: [%s: %s] %s\n", timestamp, direction, sender, mediaType, filename, content)
			} else if content != "" {
				fmt.Printf("[%s] %s %s: %s\n", timestamp, direction, sender, content)
			}
		} else if mediaType != "" {
			fmt.Printf("[%s] %s message stored (%s)\n", timestamp, direction, mediaType)
		} else {
			fmt.Printf("[%s] %s message stored\n", timestamp, direction)
		}
	}
}

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// DownloadMediaResponse represents the response for the download media API
type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

// Store additional media info in the database
func (store *MessageStore) StoreMediaInfo(id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// Get media info from the database
func (store *MessageStore) GetMediaInfo(id, chatJID string) (string, string, string, []byte, []byte, []byte, uint64, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64

	err := store.db.QueryRow(
		"SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)

	return mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err
}

// MediaDownloader implements the whatsmeow.DownloadableMessage interface
type MediaDownloader struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileLength    uint64
	FileSHA256    []byte
	FileEncSHA256 []byte
	MediaType     whatsmeow.MediaType
}

// GetDirectPath implements the DownloadableMessage interface
func (d *MediaDownloader) GetDirectPath() string {
	return d.DirectPath
}

// GetURL implements the DownloadableMessage interface
func (d *MediaDownloader) GetURL() string {
	return d.URL
}

// GetMediaKey implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaKey() []byte {
	return d.MediaKey
}

// GetFileLength implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileLength() uint64 {
	return d.FileLength
}

// GetFileSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileSHA256() []byte {
	return d.FileSHA256
}

// GetFileEncSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileEncSHA256() []byte {
	return d.FileEncSHA256
}

// GetMediaType implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaType() whatsmeow.MediaType {
	return d.MediaType
}

// Function to download media from a message
func downloadMedia(client *whatsmeow.Client, messageStore *MessageStore, messageID, chatJID string) (bool, string, string, string, error) {
	// Query the database for the message
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64
	var err error

	// The chat JID becomes part of a path, so it must be a real JID
	chat, err := parseChatJID(chatJID)
	if err != nil {
		return false, "", "", "", err
	}

	// Get media info from the database
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err = messageStore.GetMediaInfo(messageID, chatJID)

	if err != nil {
		// Try to get basic info if extended info isn't available
		err = messageStore.db.QueryRow(
			"SELECT media_type, filename FROM messages WHERE id = ? AND chat_jid = ?",
			messageID, chatJID,
		).Scan(&mediaType, &filename)

		if err != nil {
			return false, "", "", "", fmt.Errorf("failed to find message: %v", err)
		}
	}

	// Check if this is a media message
	if mediaType == "" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}

	// The file name comes from the sender and is never trusted as a path:
	// it is reduced to a plain name and saved as store/<chat>/<message id>_<name>
	chatDir, localPath, err := mediaLocalPath(storeDir, chat, messageID, filename)
	if err != nil {
		return false, "", "", "", err
	}
	absPath := localPath
	localName := filepath.Base(localPath)

	// Create directory for the chat if it doesn't exist
	if err := os.MkdirAll(chatDir, 0o700); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory: %v", err)
	}

	// Check if file already exists
	if info, err := os.Lstat(localPath); err == nil {
		if !info.Mode().IsRegular() {
			return false, "", "", "", fmt.Errorf("refusing to use %s: not a regular file", localName)
		}
		return true, mediaType, localName, absPath, nil
	}

	// If we don't have all the media info we need, we can't download
	if url == "" || len(mediaKey) == 0 || len(fileSHA256) == 0 || len(fileEncSHA256) == 0 || fileLength == 0 {
		return false, "", "", "", fmt.Errorf("incomplete media information for download")
	}
	if limit := maxMediaBytes(); fileLength > uint64(limit) {
		return false, "", "", "", fmt.Errorf("media is larger than the %d MB limit", limit>>20)
	}
	if !client.IsConnected() || !client.IsLoggedIn() {
		return false, "", "", "", fmt.Errorf("not connected to WhatsApp")
	}

	// Extract direct path from URL
	directPath := extractDirectPathFromURL(url)

	// Create a downloader that implements DownloadableMessage
	var waMediaType whatsmeow.MediaType
	switch mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	downloader := &MediaDownloader{
		URL:           url,
		DirectPath:    directPath,
		MediaKey:      mediaKey,
		FileLength:    fileLength,
		FileSHA256:    fileSHA256,
		FileEncSHA256: fileEncSHA256,
		MediaType:     waMediaType,
	}

	// Download the media using whatsmeow client
	downloadCtx, cancelDownload := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelDownload()
	mediaData, err := client.Download(downloadCtx, downloader)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to download media: %v", err)
	}

	// Save privately and atomically; never executable
	tmpPath := localPath + ".part"
	if err := os.WriteFile(tmpPath, mediaData, 0o600); err != nil {
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}
	if err := os.Rename(tmpPath, localPath); err != nil {
		os.Remove(tmpPath)
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}

	fmt.Printf("Downloaded %s media (%d bytes)\n", mediaType, len(mediaData))
	return true, mediaType, localName, absPath, nil
}

// Extract direct path from a WhatsApp media URL.
// The direct path is everything after the host, INCLUDING the query string:
// the query carries the access signature (oh=, oe=), and whatsmeow appends
// "&hash=...&mms-type=..." to it. Dropping the query makes every download
// fail with HTTP 403.
// Example URL: https://mmg.whatsapp.net/v/t62.7118-24/1381_n.enc?ccb=11-4&oh=...&oe=...
func extractDirectPathFromURL(mediaURL string) string {
	parsed, err := neturl.Parse(mediaURL)
	if err != nil || parsed.Host == "" || parsed.Path == "" {
		return mediaURL // Return original URL if parsing fails
	}
	if parsed.RawQuery == "" {
		// Keep the "&hash=" that whatsmeow appends inside a query string
		return parsed.EscapedPath() + "?"
	}
	return parsed.EscapedPath() + "?" + parsed.RawQuery
}

// Start a REST API server to expose the WhatsApp client functionality
// It listens on the loopback interface only, and every request must pass
// apiGuard (see security.go). It returns an error when the port is not free.
func startRESTServer(client *whatsmeow.Client, messageStore *MessageStore, port int, token string) (*http.Server, error) {
	mux := http.NewServeMux()
	// One send at a time keeps message order predictable
	var sendMu sync.Mutex
	limiter := newSendLimiter()

	// Handler for sending messages
	mux.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		// Parse the request body
		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.Recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
			return
		}

		if req.Message == "" && req.MediaPath == "" {
			http.Error(w, "Message or media path is required", http.StatusBadRequest)
			return
		}

		if !limiter.allow(time.Now()) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(SendMessageResponse{
				Success: false,
				Message: "Send limit reached (WHATSAPP_MAX_SENDS_PER_MINUTE). Wait a minute before sending more.",
			})
			return
		}

		// Send the message
		sendMu.Lock()
		success, message := sendWhatsAppMessage(client, messageStore, req.Recipient, req.Message, req.MediaPath)
		sendMu.Unlock()
		fmt.Println("Send request handled, success:", success)
		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Send response
		json.NewEncoder(w).Encode(SendMessageResponse{
			Success: success,
			Message: message,
		})
	})

	// Handler for downloading media
	mux.HandleFunc("/api/download", func(w http.ResponseWriter, r *http.Request) {
		// Parse the request body
		var req DownloadMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.MessageID == "" || req.ChatJID == "" {
			http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
			return
		}

		// Download the media
		success, mediaType, filename, path, err := downloadMedia(client, messageStore, req.MessageID, req.ChatJID)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Handle download result
		if !success || err != nil {
			errMsg := "Unknown error"
			if err != nil {
				errMsg = err.Error()
			}

			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(DownloadMediaResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to download media: %s", errMsg),
			})
			return
		}

		// Send successful response
		json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success:  true,
			Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
			Filename: filename,
			Path:     path,
		})
	})

	// Report whether the bridge is connected and logged in
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]bool{
			"connected": client.IsConnected(),
			"logged_in": client.IsLoggedIn(),
		})
	})

	// Bind first, so a busy port is an error instead of a silent half-start
	serverAddr := fmt.Sprintf("127.0.0.1:%d", port)
	listener, err := net.Listen("tcp4", serverAddr)
	if err != nil {
		return nil, err
	}

	server := &http.Server{
		Handler:           apiGuard(token, port, mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	fmt.Printf("Starting REST API server on %s...\n", serverAddr)

	// Run server in a goroutine so it doesn't block
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()
	return server, nil
}

func main() {
	os.Exit(runBridge())
}

func runBridge() (exitCode int) {
	exitCode = 1
	// Set up logger
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")

	// Create database connection for storing session data
	dbLog := waLog.Stdout("Database", "INFO", true)

	// Fail early on a bad port setting
	port, err := apiPort()
	if err != nil {
		logger.Errorf("%v", err)
		return
	}

	// Optional restriction on who may be messaged; a bad value stops startup
	allowedRecipients, err = parseAllowedRecipients(os.Getenv("WHATSAPP_ALLOWED_RECIPIENTS"))
	if err != nil {
		logger.Errorf("%v", err)
		return
	}
	if len(allowedRecipients) > 0 {
		logger.Infof("Sending is restricted to %d allowed recipient(s)", len(allowedRecipients))
	}

	// Create the store folder if needed and make it, and everything in it,
	// readable by this user only: it holds session keys and message history
	if err := secureStore(storeDir); err != nil {
		logger.Errorf("Failed to prepare store directory: %v", err)
		return
	}

	container, err := sqlstore.New(context.Background(), "sqlite3", "file:store/whatsapp.db?_foreign_keys=on&_busy_timeout=5000", dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if err == sql.ErrNoRows {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	// Keep the repaired IPv4 route and normal TLS verification for login and reconnects.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialer := net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
		return dialer.DialContext(ctx, "tcp4", address)
	}
	httpClient := &http.Client{Timeout: 20 * time.Second, Transport: transport}

	// Refresh the advertised public Web version before attempting the connection.
	fmt.Println("WHATSAPP_PHASE refreshing-client-version")
	latest, versionErr := whatsmeow.GetLatestVersion(context.Background(), httpClient)
	if versionErr != nil {
		fmt.Println("WHATSAPP_PHASE version-refresh-failed")
		return
	}
	store.SetWAVersion(*latest)
	fmt.Println("WHATSAPP_PHASE client-version-current")

	// Create client instance
	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}
	client.SetPreLoginHTTPClient(httpClient)
	client.SetWebsocketHTTPClient(httpClient)
	defer client.Disconnect()

	// Initialize message store
	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer messageStore.Close()

	// The databases exist now; make sure they are private as well
	if err := secureStore(storeDir); err != nil {
		logger.Errorf("Failed to secure store directory: %v", err)
		return
	}

	// Signalled when the phone removes this linked device
	loggedOut := make(chan struct{}, 1)

	// Setup event handling for messages and history sync
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			// Process regular messages
			handleMessage(client, messageStore, v, logger)

		case *events.HistorySync:
			// Process history sync events
			handleHistorySync(client, messageStore, v, logger)

		case *events.Connected:
			fmt.Println("WHATSAPP_PHASE authenticated")
			logger.Infof("Connected to WhatsApp")

		case *events.LoggedOut:
			fmt.Println("WHATSAPP_PHASE logged-out")
			notifyDesktop(noticeUnlinked, runtime.GOOS)
			select {
			case loggedOut <- struct{}{}:
			default:
			}
			logger.Warnf("This device was removed from the WhatsApp account. Start the bridge again to scan a new QR code")
		}
	})

	// Connect to WhatsApp and wait for pairing when this device has no session.
	pairedThisRun := false
	if client.Store.ID == nil {
		qrContext, cancelQR := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancelQR()
		qrChan, qrErr := client.GetQRChannel(qrContext)
		if qrErr != nil {
			fmt.Println("WHATSAPP_PHASE qr-channel-failed")
			return
		}
		fmt.Println("WHATSAPP_PHASE connecting")
		if err = client.Connect(); err != nil {
			fmt.Println("WHATSAPP_PHASE connect-failed")
			return
		}
		fmt.Println("WHATSAPP_PHASE transport-connected")

		// Only the newest QR image is kept, and none after pairing ends
		lastQRFile := ""
		removeQRFile := func() {
			if lastQRFile != "" {
				os.Remove(lastQRFile)
				lastQRFile = ""
			}
		}
		defer removeQRFile()

		// WHATSAPP_QR_PAGE=1 shows the code on a local page in the browser
		var page *pairingPage
		if os.Getenv("WHATSAPP_QR_PAGE") == "1" {
			if page, err = newPairingPage(); err != nil {
				fmt.Println("WHATSAPP_PHASE qr-page-failed")
				page = nil
			}
		}
		defer page.close()

		// Ctrl+C or a stop request during pairing must still clean up the
		// QR image and the pairing page, so it is handled here.
		interrupted := make(chan os.Signal, 1)
		signal.Notify(interrupted, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(interrupted)

	pairing:
		for {
			select {
			case <-interrupted:
				fmt.Println("WHATSAPP_PHASE pairing-cancelled")
				return
			case <-qrContext.Done():
				fmt.Println("WHATSAPP_PHASE qr-timeout")
				return
			case evt, ok := <-qrChan:
				if !ok {
					fmt.Println("WHATSAPP_PHASE qr-channel-closed")
					return
				}
				fmt.Println("WHATSAPP_QR_EVENT " + evt.Event)
				switch evt.Event {
				case "code":
					fmt.Println("\nScan this QR code with your WhatsApp app:")
					if page != nil {
						if showErr := page.show(evt.Code); showErr != nil {
							fmt.Println("WHATSAPP_PHASE qr-page-failed")
							return
						}
					} else if prefix := os.Getenv("WHATSAPP_QR_OUTPUT"); prefix != "" {
						code, encodeErr := qr.Encode(evt.Code, qr.L)
						if encodeErr != nil {
							fmt.Println("WHATSAPP_PHASE qr-encode-failed")
							return
						}
						code.Scale = 8
						output := fmt.Sprintf("%s-%d.png", prefix, time.Now().UnixNano())
						removeQRFile()
						if writeErr := os.WriteFile(output, code.PNG(), 0600); writeErr != nil {
							fmt.Println("WHATSAPP_PHASE qr-write-failed")
							return
						}
						lastQRFile = output
						fmt.Println("PAIRING_QR_READY " + output)
					} else {
						qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
					}
				case "success":
					removeQRFile()
					page.close()
					pairedThisRun = true
					break pairing
				case "err-scanned-without-multidevice":
					continue
				case "timeout":
					fmt.Println("WHATSAPP_PHASE qr-timeout")
					notifyDesktop(noticeQRTimeout, runtime.GOOS)
					return
				default:
					fmt.Println("WHATSAPP_PHASE pairing-failed")
					return
				}
			}
		}
	} else {
		fmt.Println("WHATSAPP_PHASE connecting")
		if err = client.Connect(); err != nil {
			fmt.Println("WHATSAPP_PHASE connect-failed")
			return
		}
		fmt.Println("WHATSAPP_PHASE transport-connected")
	}

	// A socket or paired device ID alone does not prove authentication completed.
	// Right after pairing, WhatsApp closes the connection and the client
	// reconnects, so the state is polled instead of being checked once.
	if !waitUntil(45*time.Second, func() bool { return client.IsConnected() && client.IsLoggedIn() }) {
		fmt.Println("WHATSAPP_PHASE authentication-timeout")
		return
	}

	fmt.Println("\n✓ Connected to WhatsApp!")
	if pairedThisRun {
		// Tell the person who just scanned the code that it worked
		fmt.Println("WHATSAPP_PHASE paired")
		notifyDesktop(noticePaired, runtime.GOOS)
	}

	// The API token lives in the store; the MCP server reads it from there
	token, err := loadOrCreateAPIToken(storeDir)
	if err != nil {
		logger.Errorf("Failed to prepare API token: %v", err)
		return
	}

	// Start REST API server
	server, err := startRESTServer(client, messageStore, port, token)
	if err != nil {
		fmt.Println("WHATSAPP_PHASE api-listen-failed")
		logger.Errorf("Could not start the local API on 127.0.0.1:%d: %v. Is another bridge already running? Set WHATSAPP_BRIDGE_PORT to use a different port.", port, err)
		return
	}
	fmt.Println("WHATSAPP_PHASE api-ready")

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Wait for a termination signal, or for the phone to unlink this device
	unlinked := false
	select {
	case <-exitChan:
	case <-loggedOut:
		// Without a session the bridge can do nothing useful. Exiting makes
		// that visible; the next start shows a new QR code.
		unlinked = true
		time.Sleep(time.Second) // let the notification go out
	}

	fmt.Println("Disconnecting...")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	server.Shutdown(shutdownCtx)
	// Disconnect client
	client.Disconnect()
	if unlinked {
		return 1
	}
	return 0
}

// waitUntil polls ready until it reports true or the timeout passes.
func waitUntil(timeout time.Duration, ready func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if ready() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// logMessageBodies reports whether message text may be written to the log.
func logMessageBodies() bool {
	return os.Getenv("WHATSAPP_LOG_MESSAGES") == "1"
}

// senderUser reduces a sender JID such as 9725...@s.whatsapp.net or
// 1234...:12@lid to its user part, the form live messages are stored in.
func senderUser(raw string) string {
	if jid, err := types.ParseJID(raw); err == nil && jid.User != "" {
		return jid.User
	}
	if idx := strings.Index(raw, "@"); idx > 0 {
		return raw[:idx]
	}
	return raw
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	// First, check if chat already exists in database with a name
	var existingName string
	err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
	if err == nil && existingName != "" {
		// Chat exists with a name, use that
		return existingName
	}

	// Need to determine chat name
	var name string

	if jid.Server == "g.us" {
		// This is a group chat

		// Use conversation data if provided (from history sync)
		if conversation != nil {
			// Extract name from conversation if available
			// This uses type assertions to handle different possible types
			var displayName, convName *string
			// Try to extract the fields we care about regardless of the exact type
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()

				// Try to find DisplayName field
				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				// Try to find Name field
				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}

			// Use the name we found
			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}

		// If we didn't get a name, try group info
		if name == "" {
			groupCtx, cancelGroup := context.WithTimeout(context.Background(), 15*time.Second)
			groupInfo, err := client.GetGroupInfo(groupCtx, jid)
			cancelGroup()
			if err == nil && groupInfo != nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				// Fallback name for groups
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}

	} else {
		// This is an individual contact

		// Just use contact info (full name)
		contact, err := client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.FullName != "" {
			name = contact.FullName
		} else if sender != "" {
			// Fallback to sender
			name = sender
		} else {
			// Last fallback to JID
			name = jid.User
		}

	}

	return name
}

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		chatJID := *conversation.ID

		// Try to parse the JID
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", chatJID, err)
			continue
		}

		// Get appropriate chat name by passing the history sync conversation directly
		name := GetChatName(client, messageStore, jid, chatJID, conversation, "", logger)

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			timestamp := time.Time{}
			if ts := latestMsg.Message.GetMessageTimestamp(); ts != 0 {
				timestamp = time.Unix(int64(ts), 0)
			} else {
				continue
			}

			if err := messageStore.StoreChat(chatJID, name, timestamp); err != nil {
				logger.Warnf("Failed to store chat: %v", err)
				continue
			}

			// Store messages
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				// Extract text content
				var content string
				if msg.Message.Message != nil {
					if conv := msg.Message.Message.GetConversation(); conv != "" {
						content = conv
					} else if ext := msg.Message.Message.GetExtendedTextMessage(); ext != nil {
						content = ext.GetText()
					}
				}

				// Extract media info
				var mediaType, filename, url string
				var mediaKey, fileSHA256, fileEncSHA256 []byte
				var fileLength uint64

				if msg.Message.Message != nil {
					mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message)
				}

				// Skip messages with no content and no media
				if content == "" && mediaType == "" {
					continue
				}

				// Determine sender
				var sender string
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
						sender = senderUser(*msg.Message.Key.Participant)
					} else if isFromMe {
						if ownID := client.Store.ID; ownID != nil {
							sender = ownID.User
						}
					} else {
						sender = jid.User
					}
				} else {
					sender = jid.User
				}

				// Store message
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				// Get message timestamp
				timestamp := time.Time{}
				if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
					timestamp = time.Unix(int64(ts), 0)
				} else {
					continue
				}

				err = messageStore.StoreMessage(
					msgID,
					chatJID,
					sender,
					content,
					timestamp,
					isFromMe,
					mediaType,
					filename,
					url,
					mediaKey,
					fileSHA256,
					fileEncSHA256,
					fileLength,
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					syncedCount++
					if logMessageBodies() {
						logger.Infof("Stored message: [%s] %s -> %s: [%s] %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, mediaType, content)
					}
				}
			}
		}
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
}

// Request history sync from the server
func requestHistorySync(client *whatsmeow.Client) {
	if client == nil {
		fmt.Println("Client is not initialized. Cannot request history sync.")
		return
	}

	if !client.IsConnected() {
		fmt.Println("Client is not connected. Please ensure you are connected to WhatsApp first.")
		return
	}

	if client.Store.ID == nil {
		fmt.Println("Client is not logged in. Please scan the QR code first.")
		return
	}

	// Build and send a history sync request
	historyMsg := client.BuildHistorySyncRequest(nil, 100)
	if historyMsg == nil {
		fmt.Println("Failed to build history sync request.")
		return
	}

	_, err := client.SendMessage(context.Background(), types.JID{
		Server: "s.whatsapp.net",
		User:   "status",
	}, historyMsg)

	if err != nil {
		fmt.Printf("Failed to request history sync: %v\n", err)
	} else {
		fmt.Println("History sync requested. Waiting for server response...")
	}
}

// analyzeOggOpus tries to extract duration and generate a simple waveform from an Ogg Opus file
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	// Try to detect if this is a valid Ogg file by checking for the "OggS" signature
	// at the beginning of the file
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	// Parse Ogg pages to find the last page with a valid granule position
	var lastGranule uint64
	var sampleRate uint32 = 48000 // Default Opus sample rate
	var preSkip uint16 = 0
	var foundOpusHead bool

	// Scan through the file looking for Ogg pages
	for i := 0; i < len(data); {
		// Check if we have enough data to read Ogg page header
		if i+27 >= len(data) {
			break
		}

		// Verify Ogg page signature
		if string(data[i:i+4]) != "OggS" {
			// Skip until next potential page
			i++
			continue
		}

		// Extract header fields
		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		// Extract segment table
		if i+27+numSegments >= len(data) {
			break
		}
		segmentTable := data[i+27 : i+27+numSegments]

		// Calculate page size
		pageSize := 27 + numSegments
		for _, segLen := range segmentTable {
			pageSize += int(segLen)
		}

		// Check if we're looking at an OpusHead packet (should be in first few pages)
		if !foundOpusHead && pageSeqNum <= 1 {
			// Look for "OpusHead" marker in this page
			pageData := data[i : i+pageSize]
			headPos := bytes.Index(pageData, []byte("OpusHead"))
			if headPos >= 0 && headPos+12 < len(pageData) {
				// Found OpusHead, extract sample rate and pre-skip
				// OpusHead format: Magic(8) + Version(1) + Channels(1) + PreSkip(2) + SampleRate(4) + ...
				headPos += 8 // Skip "OpusHead" marker
				// PreSkip is 2 bytes at offset 10
				if headPos+12 <= len(pageData) {
					preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
					sampleRate = binary.LittleEndian.Uint32(pageData[headPos+12 : headPos+16])
					foundOpusHead = true
					fmt.Printf("Found OpusHead: sampleRate=%d, preSkip=%d\n", sampleRate, preSkip)
				}
			}
		}

		// Keep track of last valid granule position
		if granulePos != 0 {
			lastGranule = granulePos
		}

		// Move to next page
		i += pageSize
	}

	if !foundOpusHead {
		fmt.Println("Warning: OpusHead not found, using default values")
	}

	// Calculate duration based on granule position
	if lastGranule > 0 {
		// Formula for duration: (lastGranule - preSkip) / sampleRate
		durationSeconds := float64(lastGranule-uint64(preSkip)) / float64(sampleRate)
		duration = uint32(math.Ceil(durationSeconds))
		fmt.Printf("Calculated Opus duration from granule: %f seconds (lastGranule=%d)\n",
			durationSeconds, lastGranule)
	} else {
		// Fallback to rough estimation if granule position not found
		fmt.Println("Warning: No valid granule position found, using estimation")
		durationEstimate := float64(len(data)) / 2000.0 // Very rough approximation
		duration = uint32(durationEstimate)
	}

	// Make sure we have a reasonable duration (at least 1 second, at most 300 seconds)
	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	// Generate waveform
	waveform = placeholderWaveform(duration)

	fmt.Printf("Ogg Opus analysis: size=%d bytes, calculated duration=%d sec, waveform=%d bytes\n",
		len(data), duration, len(waveform))

	return duration, waveform, nil
}

// placeholderWaveform generates a synthetic waveform for WhatsApp voice messages
// that appears natural with some variability based on the duration
func placeholderWaveform(duration uint32) []byte {
	// WhatsApp expects a 64-byte waveform for voice messages
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	// Seed the random number generator for consistent results with the same duration
	rng := rand.New(rand.NewSource(int64(duration)))

	// Create a more natural looking waveform with some patterns and variability
	// rather than completely random values

	// Base amplitude and frequency - longer messages get faster frequency
	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		// Position in the waveform (normalized 0-1)
		pos := float64(i) / float64(waveformLength)

		// Create a wave pattern with some randomness
		// Use multiple sine waves of different frequencies for more natural look
		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)

		// Add some randomness to make it look more natural
		val += (rng.Float64() - 0.5) * 15

		// Add some fade-in and fade-out effects
		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)

		// Center around 50 (typical voice baseline)
		val = val + 50

		// Ensure values stay within WhatsApp's expected range (0-100)
		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}

		waveform[i] = byte(val)
	}

	return waveform
}
