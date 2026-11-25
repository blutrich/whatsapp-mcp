package main

// Names and identities for one-to-one chats.
//
// WhatsApp addresses a person by phone number (…@s.whatsapp.net) or by a
// privacy identifier (…@lid). The address book is keyed by phone number, so a
// chat that arrives under an identifier needs the mapping between the two to
// get its saved name, and a message sent by phone number belongs in the chat
// that already holds the conversation.

import (
	"context"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

func isDirectChat(jid types.JID) bool {
	return jid.Server == types.DefaultUserServer || jid.Server == types.HiddenUserServer
}

// looksLikeNumber reports whether a stored chat name is only a phone number
// or identifier, which is what the bridge stores when it knows no name.
func looksLikeNumber(name string) bool {
	cleaned := strings.NewReplacer(" ", "", "-", "", "+", "").Replace(strings.TrimSpace(name))
	return cleaned == "" || isDigits(cleaned)
}

// pickContactName chooses the best available name for a contact:
// the name saved in the address book, then the business name, then the name
// the person chose for themselves.
func pickContactName(info types.ContactInfo) string {
	for _, candidate := range []string{info.FullName, info.FirstName, info.BusinessName, info.PushName} {
		if candidate = strings.TrimSpace(candidate); candidate != "" && !looksLikeNumber(candidate) {
			return candidate
		}
	}
	return ""
}

// counterpartJID returns the other address of the same person (identifier for
// a phone number, phone number for an identifier), or an empty JID.
func counterpartJID(ctx context.Context, client *whatsmeow.Client, jid types.JID) types.JID {
	if client == nil || client.Store == nil || client.Store.LIDs == nil {
		return types.EmptyJID
	}
	var other types.JID
	var err error
	switch jid.Server {
	case types.HiddenUserServer:
		other, err = client.Store.LIDs.GetPNForLID(ctx, jid.ToNonAD())
	case types.DefaultUserServer:
		other, err = client.Store.LIDs.GetLIDForPN(ctx, jid.ToNonAD())
	default:
		return types.EmptyJID
	}
	if err != nil || other.IsEmpty() {
		return types.EmptyJID
	}
	return other.ToNonAD()
}

// bestContactName looks a person up under both of their addresses.
func bestContactName(ctx context.Context, client *whatsmeow.Client, jid types.JID) string {
	if client == nil || client.Store == nil || client.Store.Contacts == nil {
		return ""
	}
	for _, candidate := range []types.JID{jid.ToNonAD(), counterpartJID(ctx, client, jid)} {
		if candidate.IsEmpty() {
			continue
		}
		if info, err := client.Store.Contacts.GetContact(ctx, candidate); err == nil && info.Found {
			if name := pickContactName(info); name != "" {
				return name
			}
		}
	}
	return ""
}

func chatExists(messageStore *MessageStore, jid string) bool {
	var found string
	return messageStore.db.QueryRow("SELECT jid FROM chats WHERE jid = ?", jid).Scan(&found) == nil
}

// conversationJID returns the chat a message to recipient should be saved in:
// the chat that already exists for that person, preferring the identifier
// chat, which is where WhatsApp delivers their replies.
func conversationJID(ctx context.Context, client *whatsmeow.Client, messageStore *MessageStore, recipient types.JID) types.JID {
	if !isDirectChat(recipient) {
		return recipient
	}
	other := counterpartJID(ctx, client, recipient)
	if other.IsEmpty() {
		return recipient
	}
	lid, phone := recipient, other
	if recipient.Server == types.DefaultUserServer {
		lid, phone = other, recipient
	}
	if chatExists(messageStore, lid.String()) {
		return lid
	}
	if chatExists(messageStore, phone.String()) {
		return phone
	}
	return recipient
}

// mergeChat moves every message of one chat into another and removes the
// emptied chat. A message already present in the target is kept once.
func mergeChat(messageStore *MessageStore, from, into string) error {
	tx, err := messageStore.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE OR IGNORE messages SET chat_jid = ? WHERE chat_jid = ?", into, from); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM messages WHERE chat_jid = ?", from); err != nil {
		return err
	}
	// Keep the later of the two "last message" times. Plain text comparison
	// would be wrong across time zones, so SQLite converts both to UTC.
	if _, err := tx.Exec(`
		UPDATE chats SET last_message_time = (SELECT last_message_time FROM chats WHERE jid = ?)
		WHERE jid = ?
		  AND datetime((SELECT last_message_time FROM chats WHERE jid = ?)) > datetime(COALESCE(last_message_time, '0001-01-01'))`,
		from, into, from); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM chats WHERE jid = ?", from); err != nil {
		return err
	}
	return tx.Commit()
}

// repairDirectChats tidies one-to-one chats stored by earlier versions:
// chats named with a number get the saved contact name, and a conversation
// split between a phone-number chat and an identifier chat is joined into
// the identifier chat. It reports how many chats were renamed and merged.
func repairDirectChats(ctx context.Context, client *whatsmeow.Client, messageStore *MessageStore) (renamed int, merged int, err error) {
	rows, err := messageStore.db.Query("SELECT jid, COALESCE(name, '') FROM chats WHERE jid LIKE '%@s.whatsapp.net' OR jid LIKE '%@lid'")
	if err != nil {
		return 0, 0, err
	}
	type chatRow struct{ jid, name string }
	var chats []chatRow
	for rows.Next() {
		var row chatRow
		if err := rows.Scan(&row.jid, &row.name); err != nil {
			rows.Close()
			return 0, 0, err
		}
		chats = append(chats, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	for _, chat := range chats {
		jid, parseErr := types.ParseJID(chat.jid)
		if parseErr != nil || !isDirectChat(jid) {
			continue
		}
		// Join a phone-number chat into the person's identifier chat.
		if jid.Server == types.DefaultUserServer {
			if lid := counterpartJID(ctx, client, jid); !lid.IsEmpty() && chatExists(messageStore, lid.String()) {
				if err := mergeChat(messageStore, chat.jid, lid.String()); err != nil {
					return renamed, merged, err
				}
				merged++
				continue
			}
		}
		if looksLikeNumber(chat.name) {
			if name := bestContactName(ctx, client, jid); name != "" {
				if _, err := messageStore.db.Exec("UPDATE chats SET name = ? WHERE jid = ?", name, chat.jid); err != nil {
					return renamed, merged, err
				}
				renamed++
			}
		}
	}
	return renamed, merged, nil
}
