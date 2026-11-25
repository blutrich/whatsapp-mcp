package main

// Bridge status for the /api/health endpoint and the bridge_status tool.
// It reports state and settings only: no message content, names, numbers,
// paths or secrets.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
)

type bridgeStats struct {
	mu             sync.Mutex
	startedAt      time.Time
	lastReceivedAt time.Time
	receivedCount  int
	sentCount      int
	recentSends    []time.Time
	sendLimit      int
}

var stats = &bridgeStats{startedAt: time.Now()}

func (s *bridgeStats) recordReceived(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receivedCount++
	if at.After(s.lastReceivedAt) {
		s.lastReceivedAt = at
	}
}

func (s *bridgeStats) recordSend() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sentCount++
	s.recentSends = append(s.recentSends, time.Now())
}

type statusReport struct {
	Connected            bool   `json:"connected"`
	LoggedIn             bool   `json:"logged_in"`
	StartedAt            string `json:"started_at"`
	LastMessageReceived  string `json:"last_message_received_at,omitempty"`
	ReceivedSinceStart   int    `json:"messages_received_since_start"`
	SentSinceStart       int    `json:"messages_sent_since_start"`
	SentInLastMinute     int    `json:"messages_sent_in_last_minute"`
	SendLimitPerMinute   int    `json:"send_limit_per_minute"`
	ConfirmSends         bool   `json:"approval_dialog_before_sending"`
	RecipientsRestricted bool   `json:"recipients_restricted"`
	AllowedRecipients    int    `json:"allowed_recipient_count"`
	FoldersRestricted    bool   `json:"outgoing_folders_restricted"`
	MaxMediaMB           int64  `json:"max_media_mb"`
}

func (s *bridgeStats) snapshot(connected, loggedIn bool) statusReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	kept := s.recentSends[:0]
	for _, at := range s.recentSends {
		if now.Sub(at) < time.Minute {
			kept = append(kept, at)
		}
	}
	s.recentSends = kept
	report := statusReport{
		Connected:            connected,
		LoggedIn:             loggedIn,
		StartedAt:            s.startedAt.Format(time.RFC3339),
		ReceivedSinceStart:   s.receivedCount,
		SentSinceStart:       s.sentCount,
		SentInLastMinute:     len(s.recentSends),
		SendLimitPerMinute:   s.sendLimit,
		ConfirmSends:         confirmSendsEnabled(),
		RecipientsRestricted: len(allowedRecipients) > 0,
		AllowedRecipients:    len(allowedRecipients),
		FoldersRestricted:    len(mediaRoots()) > 0,
		MaxMediaMB:           maxMediaBytes() >> 20,
	}
	if !s.lastReceivedAt.IsZero() {
		report.LastMessageReceived = s.lastReceivedAt.Format(time.RFC3339)
	}
	return report
}

// describeSend works out what an approval dialog should show for a request.
// It applies the same recipient and file rules as the send itself, so the
// person is never asked to approve something the bridge would refuse anyway.
func describeSend(ctx context.Context, client *whatsmeow.Client, messageStore *MessageStore, req SendMessageRequest) (sendSummary, error) {
	recipient, err := parseRecipient(req.Recipient)
	if err != nil {
		return sendSummary{}, err
	}
	if !recipientAllowed(recipient, allowedRecipients) {
		return sendSummary{}, errors.New("this recipient is not in WHATSAPP_ALLOWED_RECIPIENTS, so the bridge will not send to it")
	}
	summary := sendSummary{recipient: recipient.String(), text: req.Message}
	if isDirectChat(recipient) {
		summary.recipientName = bestContactName(ctx, client, recipient)
	} else {
		var name string
		if messageStore.db.QueryRow("SELECT COALESCE(name, '') FROM chats WHERE jid = ?", recipient.String()).Scan(&name) == nil {
			summary.recipientName = name
		}
	}
	if req.MediaPath != "" {
		resolved, err := validateOutgoingFile(req.MediaPath, storeDir, mediaRoots(), maxMediaBytes())
		if err != nil {
			return sendSummary{}, err
		}
		summary.fileName = filepath.Base(req.MediaPath)
		if info, statErr := os.Stat(resolved); statErr == nil {
			summary.fileBytes = info.Size()
		}
	}
	return summary, nil
}
