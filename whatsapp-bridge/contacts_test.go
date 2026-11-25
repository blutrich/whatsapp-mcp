package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func TestPickContactNameAndNumberDetection(t *testing.T) {
	cases := []struct {
		info types.ContactInfo
		want string
	}{
		{types.ContactInfo{FullName: "Coach", FirstName: "C", PushName: "Ron"}, "Coach"},
		{types.ContactInfo{FirstName: "Dana", PushName: "D"}, "Dana"},
		{types.ContactInfo{BusinessName: "Gym Ltd", PushName: "gym"}, "Gym Ltd"},
		{types.ContactInfo{PushName: "Ron Trainer"}, "Ron Trainer"},
		{types.ContactInfo{FullName: "  ", PushName: "+972 50-000-0077"}, ""},
		{types.ContactInfo{}, ""},
	}
	for _, tc := range cases {
		if got := pickContactName(tc.info); got != tc.want {
			t.Errorf("pickContactName(%+v) = %q, want %q", tc.info, got, tc.want)
		}
	}
	for name, want := range map[string]bool{"": true, "972500000077": true, "+972 50-000-0077": true, "236098865373273": true, "Coach": false, "Dana 2": false, "מאמן": false} {
		if got := looksLikeNumber(name); got != want {
			t.Errorf("looksLikeNumber(%q) = %v, want %v", name, got, want)
		}
	}
}

// contactsFixture is a message store plus a WhatsApp client whose address
// book knows "Coach" by phone number and by privacy identifier.
type contactsFixture struct {
	client *whatsmeow.Client
	store  *MessageStore
	phone  types.JID
	lid    types.JID
}

func newContactsFixture(t *testing.T) contactsFixture {
	t.Helper()
	t.Chdir(t.TempDir())
	if err := secureStore(storeDir); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	container, err := sqlstore.New(ctx, "sqlite3", "file:store/whatsapp.db?_foreign_keys=on", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { container.Close() })
	device := container.NewDevice()
	own := types.NewJID("972500000009", types.DefaultUserServer)
	device.ID = &own
	// The address book belongs to a stored device, so the device is saved first.
	device.Account = &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte("test"),
		AccountSignatureKey: make([]byte, 32),
		AccountSignature:    make([]byte, 64),
		DeviceSignature:     make([]byte, 64),
	}
	if err := container.PutDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	client := whatsmeow.NewClient(device, nil)

	messageStore, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { messageStore.Close() })

	fixture := contactsFixture{
		client: client,
		store:  messageStore,
		phone:  types.NewJID("972500000077", types.DefaultUserServer),
		lid:    types.NewJID("77777777777777", types.HiddenUserServer),
	}
	if err := device.Contacts.PutContactName(ctx, fixture.phone, "Coach", "Coach"); err != nil {
		t.Fatal(err)
	}
	if err := device.LIDs.PutLIDMapping(ctx, fixture.lid, fixture.phone); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestContactNameThroughEitherAddress(t *testing.T) {
	f := newContactsFixture(t)
	ctx := context.Background()
	for _, jid := range []types.JID{f.phone, f.lid} {
		if got := bestContactName(ctx, f.client, jid); got != "Coach" {
			t.Errorf("bestContactName(%s) = %q, want Coach", jid, got)
		}
	}
	stranger := types.NewJID("88888888888888", types.HiddenUserServer)
	if got := bestContactName(ctx, f.client, stranger); got != "" {
		t.Errorf("unknown identifier resolved to %q", got)
	}

	// A chat stored with a number for a name is re-resolved; a real name is kept.
	f.store.StoreChat(f.lid.String(), f.lid.User, time.Now())
	if got := GetChatName(f.client, f.store, f.lid, f.lid.String(), nil, "", nil); got != "Coach" {
		t.Errorf("GetChatName for a number-named chat = %q, want Coach", got)
	}
	f.store.StoreChat(f.lid.String(), "My Own Label", time.Now())
	if got := GetChatName(f.client, f.store, f.lid, f.lid.String(), nil, "", nil); got != "My Own Label" {
		t.Errorf("GetChatName replaced an existing name with %q", got)
	}
	if got := GetChatName(f.client, f.store, stranger, stranger.String(), nil, "", nil); got != stranger.User {
		t.Errorf("GetChatName for an unknown person = %q, want the identifier", got)
	}
}

func TestSentMessageJoinsTheExistingConversation(t *testing.T) {
	f := newContactsFixture(t)
	ctx := context.Background()

	// No chat yet: the message is saved under the address it was sent to.
	if got := conversationJID(ctx, f.client, f.store, f.phone); got != f.phone {
		t.Errorf("no existing chat: got %s, want %s", got, f.phone)
	}
	// The conversation lives under the identifier: sends by phone number join it.
	f.store.StoreChat(f.lid.String(), "Coach", time.Now())
	for _, recipient := range []types.JID{f.phone, f.lid} {
		if got := conversationJID(ctx, f.client, f.store, recipient); got != f.lid {
			t.Errorf("conversationJID(%s) = %s, want %s", recipient, got, f.lid)
		}
	}
	group := types.NewJID("120363000000000001", types.GroupServer)
	if got := conversationJID(ctx, f.client, f.store, group); got != group {
		t.Errorf("a group was redirected to %s", got)
	}
}

func TestRepairDirectChats(t *testing.T) {
	f := newContactsFixture(t)
	ctx := context.Background()
	early := time.Date(2026, 11, 22, 17, 0, 0, 0, time.FixedZone("IST", 2*3600))
	late := early.Add(2 * time.Hour)
	text := func(s string) *waProto.Message { return &waProto.Message{Conversation: proto.String(s)} }

	// The real conversation, stored under the identifier with a number for a name.
	f.store.StoreChat(f.lid.String(), f.lid.User, early)
	f.store.StoreMessage("IN1", f.lid.String(), f.lid.User, "see you thursday", early, false, "", "", "", nil, nil, nil, 0)
	// Messages sent by phone number that landed in a second chat.
	if err := recordSentMessage(f.store, f.phone.String(), "Coach", "972500000009", "OUT1", late, text("trained today"), "trained today"); err != nil {
		t.Fatal(err)
	}
	// The same message present in both chats must end up once.
	f.store.StoreMessage("IN1", f.phone.String(), f.lid.User, "see you thursday", early, false, "", "", "", nil, nil, nil, 0)
	// An unrelated number-named chat with no contact, a named chat, and a group stay as they are.
	f.store.StoreChat("88888888888888@lid", "88888888888888", early)
	f.store.StoreChat("972500000055@s.whatsapp.net", "Dana Levi", early)
	f.store.StoreChat("120363000000000001@g.us", "12345", early)

	renamed, merged, err := repairDirectChats(ctx, f.client, f.store)
	if err != nil {
		t.Fatal(err)
	}
	if renamed != 1 || merged != 1 {
		t.Errorf("renamed=%d merged=%d, want 1 and 1", renamed, merged)
	}

	rows, err := f.store.db.Query("SELECT jid, name FROM chats ORDER BY jid")
	if err != nil {
		t.Fatal(err)
	}
	var chats []string
	for rows.Next() {
		var jid, name string
		rows.Scan(&jid, &name)
		chats = append(chats, jid+"="+name)
	}
	rows.Close()
	want := "120363000000000001@g.us=12345|77777777777777@lid=Coach|88888888888888@lid=88888888888888|972500000055@s.whatsapp.net=Dana Levi"
	if got := strings.Join(chats, "|"); got != want {
		t.Errorf("chats after repair:\n%s\nwant:\n%s", got, want)
	}

	var count int
	f.store.db.QueryRow("SELECT COUNT(*) FROM messages WHERE chat_jid = ?", f.lid.String()).Scan(&count)
	if count != 2 {
		t.Errorf("identifier chat has %d messages, want 2 (IN1 once, OUT1)", count)
	}
	f.store.db.QueryRow("SELECT COUNT(*) FROM messages WHERE chat_jid = ?", f.phone.String()).Scan(&count)
	if count != 0 {
		t.Errorf("%d messages left in the removed phone-number chat", count)
	}
	var lastIsLate bool
	f.store.db.QueryRow("SELECT datetime(last_message_time) = datetime(?) FROM chats WHERE jid = ?", late, f.lid.String()).Scan(&lastIsLate)
	if !lastIsLate {
		t.Error("merged chat did not keep the later last-message time")
	}

	// Running it again changes nothing.
	renamed, merged, err = repairDirectChats(ctx, f.client, f.store)
	if err != nil || renamed != 0 || merged != 0 {
		t.Errorf("second run: renamed=%d merged=%d err=%v, want no changes", renamed, merged, err)
	}
}
