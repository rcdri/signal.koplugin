package main

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-signal/pkg/libsignalgo"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/backuppb"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/types"
)

// Messages kept per chat from a history transfer (the Kindle has little room).
const historyPerChat = 300

// importHistory waits for the phone's "transfer message history" archive
// (offered when linking) and turns it into chats and messages.
func (a *App) importHistory(ctx context.Context, cli *signalmeow.Client) {
	a.mu.Lock()
	a.syncing = "history"
	a.mu.Unlock()
	a.bus.Publish(Event{Type: "state", Text: "history"})
	done := func(ok bool) {
		if ok {
			a.st.SetKV(a.ctx, "history_done", "1")
		}
		a.mu.Lock()
		if a.syncing == "history" {
			a.syncing = ""
		}
		a.mu.Unlock()
		a.bus.Publish(Event{Type: "state", Text: "synced"})
		a.bus.Publish(Event{Type: "chats", Reload: true})
	}
	meta, err := cli.WaitForTransfer(ctx)
	if errors.Is(err, signalmeow.ErrTransferTimedOut) {
		a.log.Info().Msg("No message history was transferred")
		done(true)
		return
	} else if err != nil {
		a.log.Warn().Err(err).Msg("Waiting for history transfer failed")
		done(ctx.Err() == nil && !strings.Contains(err.Error(), "connect"))
		return
	} else if meta.Error != "" {
		a.log.Info().Str("reason", meta.Error).Msg("Phone didn't send message history")
		done(true)
		return
	}
	if err = cli.FetchAndProcessTransfer(ctx, meta); err != nil {
		a.log.Err(err).Msg("Processing history transfer failed")
		done(true)
		return
	}
	n := a.importBackupChats(ctx, cli)
	_ = cli.Store.BackupStore.ClearBackup(ctx)
	a.log.Info().Int("chats", n).Msg("Imported message history")
	done(true)
}

func (a *App) importBackupChats(ctx context.Context, cli *signalmeow.Client) int {
	bs := cli.Store.BackupStore
	chats, err := bs.GetBackupChats(ctx)
	if err != nil {
		a.log.Err(err).Msg("Reading transferred chats failed")
		return 0
	}
	recipients := map[uint64]*backuppb.Recipient{}
	getRecipient := func(id uint64) *backuppb.Recipient {
		if r, ok := recipients[id]; ok {
			return r
		}
		r, _ := bs.GetBackupRecipient(ctx, id)
		recipients[id] = r
		return r
	}
	aciOf := func(id uint64) uuid.UUID {
		r := getRecipient(id)
		switch d := r.GetDestination().(type) {
		case *backuppb.Recipient_Self:
			return cli.Store.ACI
		case *backuppb.Recipient_Contact:
			if len(d.Contact.GetAci()) == 16 {
				return uuid.UUID(d.Contact.GetAci())
			}
		}
		return uuid.Nil
	}
	count := 0
	for _, bc := range chats {
		if ctx.Err() != nil {
			break
		}
		r := getRecipient(bc.RecipientId)
		if r == nil {
			continue
		}
		var chatID, kind, title string
		switch d := r.Destination.(type) {
		case *backuppb.Recipient_Contact:
			aci := uuid.Nil
			if len(d.Contact.GetAci()) == 16 {
				aci = uuid.UUID(d.Contact.GetAci())
			}
			if aci == uuid.Nil || bc.TotalMessages == 0 {
				continue
			}
			chatID, kind = aci.String(), "dm"
		case *backuppb.Recipient_Self:
			chatID, kind = cli.Store.ACI.String(), "self"
		case *backuppb.Recipient_Group:
			if len(d.Group.MasterKey) != libsignalgo.GroupMasterKeyLength {
				continue
			}
			gid, err := libsignalgo.GroupMasterKey(d.Group.MasterKey).GroupIdentifier()
			if err != nil {
				continue
			}
			chatID, kind = base64.StdEncoding.EncodeToString(gid[:]), "group"
			title = d.Group.GetSnapshot().GetTitle().GetTitle()
			_, _ = cli.StoreMasterKey(ctx, masterKeyString(d.Group.MasterKey))
		default:
			continue
		}
		_ = a.st.EnsureChat(ctx, chatID, kind)
		if title != "" {
			a.st.SetChatName(ctx, chatID, strings.TrimSpace(title))
		}
		if bc.GetArchived() {
			_ = a.st.SetChatFlag(ctx, chatID, "archived", true)
		}
		if bc.PinnedOrder != nil && bc.GetPinnedOrder() > 0 {
			_ = a.st.SetChatFlag(ctx, chatID, "pinned", true)
		}
		if mu := bc.GetMuteUntilMs(); mu > uint64(time.Now().UnixMilli()) {
			_ = a.st.SetChatFlag(ctx, chatID, "muted", true)
		}
		if t := bc.GetExpirationTimerMs(); t > 0 || bc.GetExpireTimerVersion() > 0 {
			a.st.SetChatTimer(ctx, chatID, int(t/1000), int(bc.GetExpireTimerVersion()))
		}
		items, err := bs.GetBackupChatItems(ctx, bc.Id, time.Time{}, false, historyPerChat)
		if err != nil {
			continue
		}
		unread := 0
		for _, item := range items {
			m, atts := a.convertBackupItem(chatID, item, aciOf)
			if m == nil {
				continue
			}
			if inc, ok := item.DirectionalDetails.(*backuppb.ChatItem_Incoming); ok && !inc.Incoming.GetRead() && !m.FromMe {
				unread++
			}
			_ = a.st.PutMsg(ctx, m, atts)
		}
		a.st.RefreshPreview(ctx, chatID)
		a.st.SetUnread(ctx, chatID, unread)
		a.names.ensureChatName(chatID, kind)
		count++
	}
	return count
}

func masterKeyString(b []byte) types.SerializedGroupMasterKey {
	return types.SerializedGroupMasterKey(base64.StdEncoding.EncodeToString(b))
}

func (a *App) convertBackupItem(chat string, item *backuppb.ChatItem, aciOf func(uint64) uuid.UUID) (*Msg, []byte) {
	author := aciOf(item.AuthorId)
	if author == uuid.Nil {
		return nil, nil
	}
	if _, ok := item.Item.(*backuppb.ChatItem_RemoteDeletedMessage); ok {
		return &Msg{ID: msgID(author, item.DateSent), Chat: chat, TS: int64(item.DateSent), Sender: author.String(),
			FromMe: author == a.myACI(), Kind: "deleted"}, nil
	}
	attMap := make(AttachmentMap)
	dm, reactions := BackupToDataMessage(item, attMap)
	if dm == nil {
		return nil, nil
	}
	dm.Timestamp = proto.Uint64(item.DateSent)
	if sm := item.GetStandardMessage(); sm != nil && sm.Quote != nil {
		q := sm.Quote
		qa := aciOf(q.AuthorId)
		if qa != uuid.Nil {
			dm.Quote = &signalpb.DataMessage_Quote{
				Id:              proto.Uint64(q.GetTargetSentTimestamp()),
				AuthorAciBinary: qa[:],
				Text:            proto.String(q.GetText().GetBody()),
			}
		}
	}
	hashes := map[int][]byte{}
	for i, att := range dm.Attachments {
		if len(att.ClientUuid) == 16 {
			if li := attMap[uuid.UUID(att.ClientUuid)]; li != nil {
				hashes[i] = li.GetPlaintextHash()
			}
		}
	}
	m, atts := a.convertDataMessage(chat, author, dm, hashes)
	if m == nil {
		return nil, nil
	}
	for _, r := range reactions {
		if by := aciOf(r.AuthorId); by != uuid.Nil {
			m.Reactions = append(m.Reactions, Reaction{Emoji: r.GetEmoji(), By: by.String(), TS: int64(r.GetSentTimestamp())})
		}
	}
	if len(item.Revisions) > 0 {
		m.Edited = true
	}
	if out, ok := item.DirectionalDetails.(*backuppb.ChatItem_Outgoing); ok {
		m.Status = "sent"
		for _, s := range out.Outgoing.GetSendStatus() {
			switch {
			case s.GetRead() != nil || s.GetViewed() != nil:
				m.Status = "read"
			case s.GetDelivered() != nil && m.Status != "read":
				m.Status = "delivered"
			}
		}
	}
	if exp := item.GetExpiresInMs(); exp > 0 {
		m.ExpiresIn = int(exp / 1000)
		if start := item.GetExpireStartDate(); start > 0 {
			m.ExpiresAt = int64(start + exp)
		}
	}
	return m, atts
}
