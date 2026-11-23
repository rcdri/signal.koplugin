package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	_ "go.mau.fi/util/dbutil/litestream"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-signal/pkg/libsignalgo"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/events"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/store"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/types"
)

type App struct {
	log        zerolog.Logger
	dataDir    string
	deviceName string
	db         *dbutil.Database
	container  *store.Container
	st         *Store
	bus        *Bus
	ctx        context.Context
	cancel     context.CancelFunc

	mu          sync.Mutex
	state       string // unpaired, pairing, linking, connecting, connected, disconnected, logged_out, error
	detail      string
	qr          string
	qrSince     time.Time
	qrRounds    int
	syncing     string // "", "history", "messages"
	cli         *signalmeow.Client
	loginCancel context.CancelFunc
	connCancel  context.CancelFunc
	demo        bool

	names     *Names
	sendQueue chan func()
	lastTS    int64
	tsMu      sync.Mutex
}

const qrRotate = 45 * time.Second // Signal Desktop gets a new code after 45 s
const qrMaxRounds = 6

func NewApp(dataDir, deviceName string, log zerolog.Logger) (*App, error) {
	db, err := dbutil.NewWithDialect("file:"+filepath.Join(dataDir, "signal.db")+"?_txlock=immediate", "sqlite3-fk-wal")
	if err != nil {
		return nil, err
	}
	db.Owner = "sig-helper"
	db.RawDB.SetMaxOpenConns(1) // one writer; keeps memory low on the Kindle
	ctx, cancel := context.WithCancel(log.WithContext(context.Background()))
	a := &App{
		log:        log,
		dataDir:    dataDir,
		deviceName: deviceName,
		db:         db,
		st:         &Store{db: db},
		bus:        NewBus(),
		ctx:        ctx,
		cancel:     cancel,
		state:      "starting",
		sendQueue:  make(chan func(), 64),
	}
	a.container = store.NewStore(db, dbutil.ZeroLogger(log.With().Str("db", "signalmeow").Logger()))
	if err := a.container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrading signalmeow tables: %w", err)
	}
	if err := a.st.Init(ctx); err != nil {
		return nil, fmt.Errorf("creating tables: %w", err)
	}
	a.names = NewNames(a)
	go a.sendLoop()
	go a.expiryLoop()
	return a, nil
}

func (a *App) Close() {
	a.mu.Lock()
	cli := a.cli
	if a.loginCancel != nil {
		a.loginCancel()
	}
	a.mu.Unlock()
	if cli != nil {
		_ = cli.StopReceiveLoops()
	}
	a.cancel()
	_ = a.db.Close()
}

func (a *App) setState(state, detail string) {
	a.mu.Lock()
	changed := a.state != state || a.detail != detail
	a.state, a.detail = state, detail
	if state != "pairing" {
		a.qr = ""
	}
	a.mu.Unlock()
	if changed {
		a.log.Info().Str("state", state).Str("detail", detail).Msg("State")
		a.bus.Publish(Event{Type: "state", Text: state})
	}
}

func (a *App) client() *signalmeow.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cli
}

func (a *App) myACI() uuid.UUID {
	if a.demo {
		return demoMe
	}
	if c := a.client(); c != nil {
		return c.Store.ACI
	}
	return uuid.Nil
}

type Status struct {
	State       string `json:"state"`
	Detail      string `json:"detail,omitempty"`
	QR          string `json:"qr,omitempty"`
	QRExpiresIn int    `json:"qr_expires_in,omitempty"`
	Linked      bool   `json:"linked"`
	Syncing     string `json:"syncing,omitempty"`
	Me          *Me    `json:"me,omitempty"`
	Version     string `json:"version"`
	Demo        bool   `json:"demo,omitempty"`
}

type Me struct {
	ACI    string `json:"aci"`
	Number string `json:"number,omitempty"`
	Name   string `json:"name,omitempty"`
	Device int    `json:"device,omitempty"`
}

func (a *App) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := Status{State: a.state, Detail: a.detail, QR: a.qr, Syncing: a.syncing, Version: version, Demo: a.demo}
	if a.qr != "" {
		s.QRExpiresIn = int((qrRotate - time.Since(a.qrSince)).Seconds())
	}
	if a.demo && a.state != "unpaired" && a.state != "pairing" {
		s.Linked = true
		s.Me = &Me{ACI: demoMe.String(), Number: "+351 910 000 000", Name: "Demo"}
	}
	if a.cli != nil {
		s.Linked = true
		s.Me = &Me{ACI: a.cli.Store.ACI.String(), Number: a.cli.Store.Number, Device: a.cli.Store.DeviceID}
		s.Me.Name = a.names.cachedName(a.cli.Store.ACI)
	}
	return s
}

// ---------------------------------------------------------------- lifecycle

func (a *App) Start() {
	devices, err := a.container.GetAllDevices(a.ctx)
	if err != nil {
		a.setState("error", err.Error())
		return
	}
	if len(devices) == 0 || !devices[0].IsDeviceLoggedIn() {
		a.setState("unpaired", "")
		return
	}
	a.useDevice(devices[0])
	a.connect()
}

func (a *App) useDevice(dev *store.Device) {
	cli := signalmeow.NewClient(dev, a.log.With().Str("component", "signalmeow").Logger(), a.handleEvent)
	last, _ := strconv.ParseInt(a.st.GetKV(a.ctx, "contacts_synced"), 10, 64)
	cli.SyncContactsOnConnect = time.Since(time.Unix(last, 0)) > 3*24*time.Hour
	a.mu.Lock()
	a.cli = cli
	a.mu.Unlock()
}

func (a *App) connect() {
	cli := a.client()
	if cli == nil {
		return
	}
	a.mu.Lock()
	if a.connCancel != nil {
		a.connCancel()
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.connCancel = cancel
	a.mu.Unlock()
	a.setState("connecting", "")
	go a.connectLoop(ctx, cli)
}

func (a *App) connectLoop(ctx context.Context, cli *signalmeow.Client) {
	for retry := 0; ; retry++ {
		ch, err := cli.StartReceiveLoops(ctx)
		if err != nil {
			a.log.Err(err).Msg("Connect failed")
			a.setState("disconnected", err.Error())
			wait := time.Duration(min(2<<retry, 120)) * time.Second
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return
			}
		}
		if cli.SyncContactsOnConnect {
			a.st.SetKV(a.ctx, "contacts_synced", strconv.FormatInt(time.Now().Unix(), 10))
		}
		a.mu.Lock()
		a.syncing = "messages"
		a.mu.Unlock()
		go a.afterConnect(ctx, cli)
		fatal := a.watchStatus(ctx, cli, ch)
		if !fatal || ctx.Err() != nil {
			return
		}
		_ = cli.StopReceiveLoops()
		select {
		case <-time.After(30 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

func (a *App) watchStatus(ctx context.Context, cli *signalmeow.Client, ch chan signalmeow.SignalConnectionStatus) (fatal bool) {
	for {
		select {
		case <-ctx.Done():
			return false
		case st, ok := <-ch:
			if !ok {
				return false
			}
			switch st.Event {
			case signalmeow.SignalConnectionEventConnected:
				a.setState("connected", "")
			case signalmeow.SignalConnectionEventDisconnected:
				a.setState("disconnected", "")
			case signalmeow.SignalConnectionEventLoggedOut:
				a.loggedOut()
				return false
			case signalmeow.SignalConnectionEventError:
				detail := ""
				if st.Err != nil {
					detail = st.Err.Error()
				}
				a.setState("disconnected", detail)
			case signalmeow.SignalConnectionEventFatalError:
				detail := ""
				if st.Err != nil {
					detail = st.Err.Error()
				}
				a.setState("error", detail)
				return true
			}
		}
	}
}

func (a *App) afterConnect(ctx context.Context, cli *signalmeow.Client) {
	if cli.Store.MasterKey != nil {
		cli.QueueStorageSync(ctx)
	}
	go a.names.refreshGroups(ctx)
	if cli.Store.EphemeralBackupKey != nil && a.st.GetKV(ctx, "history_done") != "1" {
		a.importHistory(ctx, cli)
	}
}

func (a *App) Reconnect() {
	if a.client() == nil {
		return
	}
	go func() {
		if cli := a.client(); cli != nil {
			_ = cli.StopReceiveLoops()
		}
		a.connect()
	}()
}

// ---------------------------------------------------------------- linking

func (a *App) StartLogin(deviceName string) error {
	if a.demo {
		return demoLogin(a)
	}
	if a.client() != nil {
		return errors.New("already linked")
	}
	if deviceName == "" {
		deviceName = a.deviceName
	}
	a.mu.Lock()
	if a.loginCancel != nil {
		a.loginCancel()
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.loginCancel = cancel
	a.qrRounds = 0
	a.mu.Unlock()
	a.setState("pairing", "")
	go a.loginLoop(ctx, deviceName)
	return nil
}

func (a *App) CancelLogin() {
	a.mu.Lock()
	if a.loginCancel != nil {
		a.loginCancel()
		a.loginCancel = nil
	}
	linked := a.cli != nil
	a.mu.Unlock()
	if !linked {
		a.setState("unpaired", "")
	}
}

func (a *App) loginLoop(ctx context.Context, deviceName string) {
	for round := 1; round <= qrMaxRounds; round++ {
		pctx, pcancel := context.WithCancel(ctx)
		ch := signalmeow.PerformProvisioning(pctx, a.container, deviceName, true)
		done, err := a.waitProvisioning(ctx, ch)
		pcancel()
		if ctx.Err() != nil {
			return
		}
		if done {
			return
		}
		if err != nil {
			a.log.Err(err).Msg("Linking failed")
			msg := err.Error()
			var dle signalmeow.DeviceLinkError
			if errors.As(err, &dle) && dle.StatusCode == 409 {
				msg = "too_many_devices"
			}
			a.setState("unpaired", msg)
			return
		}
	}
	a.setState("unpaired", "expired")
}

// waitProvisioning shows one code; it returns done=true when linked, or
// (false, nil) when the code timed out and a new one should be fetched.
func (a *App) waitProvisioning(ctx context.Context, ch chan signalmeow.ProvisioningResponse) (bool, error) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer.C:
			return false, nil
		case resp, ok := <-ch:
			if !ok {
				return false, nil
			}
			switch {
			case resp.Err != nil:
				if errors.Is(resp.Err, context.DeadlineExceeded) || strings.Contains(resp.Err.Error(), "timeout") ||
					strings.Contains(resp.Err.Error(), "deadline") {
					return false, nil
				}
				return false, resp.Err
			case resp.State == signalmeow.StateProvisioningURLReceived:
				a.mu.Lock()
				a.qr = resp.ProvisioningURL
				a.qrSince = time.Now()
				a.qrRounds++
				a.state, a.detail = "pairing", ""
				a.mu.Unlock()
				a.log.Info().Msg("Got linking code")
				a.bus.Publish(Event{Type: "qr"})
				timer.Reset(qrRotate)
			case resp.State == signalmeow.StateProvisioningDataReceived:
				timer.Stop()
				a.setState("linking", "")
				data := resp.ProvisioningData
				if data == nil || data.ACI == uuid.Nil {
					return false, errors.New("Signal didn't send an account ID")
				}
				dev, err := a.container.DeviceByACI(a.ctx, data.ACI)
				if err != nil || dev == nil {
					return false, fmt.Errorf("loading the new device: %v", err)
				}
				_ = a.st.Wipe(a.ctx)
				a.useDevice(dev)
				a.client().SyncContactsOnConnect = true
				a.connect()
				return true, nil
			}
		}
	}
}

// ---------------------------------------------------------------- logout

func (a *App) Logout(ctx context.Context) error {
	if a.demo {
		return demoLogout(a)
	}
	cli := a.client()
	if cli != nil {
		uctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := cli.Unlink(uctx); err != nil {
			a.log.Warn().Err(err).Msg("Unlink request failed; removing local data anyway")
		}
		cancel()
	}
	a.wipeLocal()
	a.setState("unpaired", "")
	return nil
}

func (a *App) loggedOut() {
	a.log.Warn().Msg("Signal logged this device out")
	a.wipeLocal()
	a.setState("logged_out", "")
}

func (a *App) wipeLocal() {
	a.mu.Lock()
	cli := a.cli
	a.cli = nil
	if a.connCancel != nil {
		a.connCancel()
	}
	a.syncing = ""
	a.mu.Unlock()
	if cli != nil {
		_ = cli.StopReceiveLoops()
		_ = cli.Store.ClearDeviceKeys(a.ctx)
		_ = a.container.DeleteDevice(a.ctx, &cli.Store.DeviceData)
		_ = cli.Store.BackupStore.ClearBackup(a.ctx)
	}
	_ = a.st.Wipe(a.ctx)
	_ = os.RemoveAll(filepath.Join(a.dataDir, "media"))
	a.names.reset()
	a.bus.Publish(Event{Type: "chats", Reload: true})
}

// ---------------------------------------------------------------- ids

func msgID(author uuid.UUID, ts uint64) string {
	return author.String() + ":" + strconv.FormatUint(ts, 10)
}

func parseMsgID(id string) (uuid.UUID, uint64, error) {
	i := strings.LastIndexByte(id, ':')
	if i < 0 {
		return uuid.Nil, 0, errors.New("bad message id")
	}
	u, err := uuid.Parse(id[:i])
	if err != nil {
		return uuid.Nil, 0, err
	}
	ts, err := strconv.ParseUint(id[i+1:], 10, 64)
	return u, ts, err
}

func isGroupChat(id string) bool {
	if len(id) == 36 || strings.HasPrefix(id, "PNI:") {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(id)
	return err == nil && len(id) > 36
}

func (a *App) chatKind(id string) string {
	if isGroupChat(id) {
		return "group"
	}
	if id == a.myACI().String() {
		return "self"
	}
	return "dm"
}

func (a *App) nextTS() uint64 {
	a.tsMu.Lock()
	defer a.tsMu.Unlock()
	ts := time.Now().UnixMilli()
	if ts <= a.lastTS {
		ts = a.lastTS + 1
	}
	a.lastTS = ts
	return uint64(ts)
}

// ---------------------------------------------------------------- incoming

func (a *App) handleEvent(evt events.SignalEvent) bool {
	ctx := a.ctx
	switch e := evt.(type) {
	case *events.ChatEvent:
		a.handleChatEvent(ctx, e)
	case *events.Receipt:
		a.handleReceipt(ctx, e)
	case *events.ReadSelf:
		a.handleReadSelf(ctx, e)
	case *events.ContactList:
		a.names.contactsChanged(e.Contacts)
	case *events.Call:
		if e.IsRinging {
			m := &Msg{ID: msgID(e.Info.Sender, e.Timestamp), Chat: e.Info.ChatID, TS: int64(e.Timestamp),
				Sender: e.Info.Sender.String(), Kind: "call", Text: "Incoming call (answer it on your phone)"}
			a.storeIncoming(ctx, m, nil, true)
		}
	case *events.DecryptionError:
		a.log.Warn().Err(e.Err).Stringer("sender", e.Sender).Msg("Decryption error")
	case *events.QueueEmpty:
		a.mu.Lock()
		if a.syncing == "messages" {
			a.syncing = ""
		}
		a.mu.Unlock()
		a.bus.Publish(Event{Type: "state", Text: "synced"})
	case *events.LoggedOut:
		go a.loggedOut()
	case *events.DeleteForMe:
		a.handleDeleteForMe(ctx, e)
	}
	return true
}

func (a *App) handleChatEvent(ctx context.Context, e *events.ChatEvent) {
	chat := e.Info.ChatID
	switch c := e.Event.(type) {
	case *signalpb.DataMessage:
		a.handleDataMessage(ctx, chat, e.Info.Sender, c, e.Info.ServerTimestamp)
	case *signalpb.EditMessage:
		a.handleEdit(ctx, chat, e.Info.Sender, c)
	case *signalpb.TypingMessage:
		if e.Info.Sender != a.myACI() {
			a.bus.Publish(Event{Type: "typing", Chat: chat, Text: e.Info.Sender.String(),
				New: c.GetAction() == signalpb.TypingMessage_STARTED})
		}
	}
}

func (a *App) handleDataMessage(ctx context.Context, chat string, sender uuid.UUID, dm *signalpb.DataMessage, serverTS uint64) {
	me := a.myACI()
	fromMe := sender == me
	ts := dm.GetTimestamp()
	kind := a.chatKind(chat)
	_ = a.st.EnsureChat(ctx, chat, kind)
	a.names.ensureChatName(chat, kind)

	if r := dm.GetReaction(); r != nil {
		author, err := signalmeow.ParseStringOrBinaryUUID(r.GetTargetAuthorAci(), r.GetTargetAuthorAciBinary())
		if err == nil {
			a.applyReaction(ctx, chat, msgID(author, r.GetTargetSentTimestamp()), sender, r.GetEmoji(), r.GetRemove(), int64(ts))
		}
		return
	}
	if d := dm.GetDelete(); d != nil {
		a.applyDelete(ctx, chat, msgID(sender, d.GetTargetSentTimestamp()))
		return
	}
	if d := dm.GetAdminDelete(); d != nil {
		if author, err := uuid.FromBytes(d.GetTargetAuthorAciBinary()); err == nil {
			a.applyDelete(ctx, chat, msgID(author, d.GetTargetSentTimestamp()))
		}
		return
	}
	if dm.PollVote != nil || dm.PollTerminate != nil || dm.PinMessage != nil || dm.UnpinMessage != nil {
		return
	}
	// Disappearing-message timer
	if kind != "group" || dm.GetExpireTimerVersion() > 0 {
		old, oldVer := a.st.ChatTimer(ctx, chat)
		newTimer, ver := int(dm.GetExpireTimer()), int(dm.GetExpireTimerVersion())
		if ver >= oldVer && (newTimer != old || ver != oldVer) {
			a.st.SetChatTimer(ctx, chat, newTimer, ver)
			if dm.GetFlags()&uint32(signalpb.DataMessage_EXPIRATION_TIMER_UPDATE) != 0 || newTimer != old {
				text := "Disappearing messages turned off"
				if newTimer > 0 {
					text = "Disappearing messages set to " + fmtDuration(newTimer)
				}
				a.storeIncoming(ctx, &Msg{ID: msgID(sender, ts), Chat: chat, TS: int64(ts), Sender: sender.String(),
					FromMe: fromMe, Kind: "system", Text: text}, nil, false)
			}
			a.bus.Publish(Event{Type: "chat", Chat: chat})
		}
	}
	if dm.GetFlags()&uint32(signalpb.DataMessage_EXPIRATION_TIMER_UPDATE) != 0 ||
		dm.GetFlags()&1 != 0 ||
		dm.GetFlags()&uint32(signalpb.DataMessage_PROFILE_KEY_UPDATE) != 0 {
		return
	}
	if g := dm.GetGroupV2(); g != nil && len(g.GetGroupChange()) > 0 && dm.Body == nil && len(dm.Attachments) == 0 && dm.Sticker == nil {
		a.storeIncoming(ctx, &Msg{ID: msgID(sender, ts), Chat: chat, TS: int64(ts), Sender: sender.String(),
			FromMe: fromMe, Kind: "system", Text: a.names.nameOf(sender) + " updated the group"}, nil, false)
		go a.names.refreshGroup(a.ctx, chat)
		return
	}
	if dm.GroupCallUpdate != nil {
		a.storeIncoming(ctx, &Msg{ID: msgID(sender, ts), Chat: chat, TS: int64(ts), Sender: sender.String(),
			FromMe: fromMe, Kind: "call", Text: a.names.nameOf(sender) + " started a group call"}, nil, !fromMe)
		return
	}
	m, atts := a.convertDataMessage(chat, sender, dm, nil)
	if m == nil {
		return
	}
	if fromMe {
		m.Status = "sent"
	} else if t := dm.GetExpireTimer(); t > 0 {
		m.ExpiresIn = int(t)
	}
	if fromMe && dm.GetExpireTimer() > 0 {
		m.ExpiresIn = int(dm.GetExpireTimer())
		m.ExpiresAt = m.TS + int64(dm.GetExpireTimer())*1000
	}
	a.storeIncoming(ctx, m, atts, !fromMe)
	if fromMe {
		// Sending from the phone means the chat was read there
		a.st.SetUnread(ctx, chat, 0)
		a.bus.Publish(Event{Type: "chat", Chat: chat})
	}
	a.maybeFetchLongText(chat, m.ID, dm)
}

// storeIncoming saves a message, updates the chat and publishes it.
func (a *App) storeIncoming(ctx context.Context, m *Msg, atts []byte, unread bool) {
	if old, _ := a.st.GetMsg(ctx, m.Chat, m.ID); old != nil {
		// Duplicate delivery: keep reactions and status we already know
		if len(m.Reactions) == 0 {
			m.Reactions = old.Reactions
		}
		if old.Status != "" && m.Status == "" {
			m.Status = old.Status
		}
		unread = false
	}
	_ = a.st.EnsureChat(ctx, m.Chat, a.chatKind(m.Chat))
	if err := a.st.PutMsg(ctx, m, atts); err != nil {
		a.log.Err(err).Msg("Saving message failed")
		return
	}
	_ = a.st.BumpChat(ctx, m, unread)
	a.names.fill(m)
	a.bus.Publish(Event{Type: "msg", Chat: m.Chat, Msg: m, New: unread})
}

func (a *App) updateMsg(ctx context.Context, m *Msg) {
	_ = a.st.PutMsg(ctx, m, nil)
	a.names.fill(m)
	a.bus.Publish(Event{Type: "msg", Chat: m.Chat, Msg: m})
	if c, _ := a.st.GetChat(ctx, m.Chat); c != nil && c.LastTS == m.TS {
		a.st.RefreshPreview(ctx, m.Chat)
		a.bus.Publish(Event{Type: "chat", Chat: m.Chat})
	}
}

func (a *App) findMsg(ctx context.Context, chat, id string) *Msg {
	m, _ := a.st.GetMsg(ctx, chat, id)
	if m == nil && chat != "" {
		m, _ = a.st.GetMsg(ctx, "", id)
	}
	return m
}

func (a *App) applyReaction(ctx context.Context, chat, target string, by uuid.UUID, emoji string, remove bool, ts int64) {
	m := a.findMsg(ctx, chat, target)
	if m == nil {
		return
	}
	who := by.String()
	out := m.Reactions[:0]
	for _, r := range m.Reactions {
		if r.By != who {
			out = append(out, r)
		}
	}
	m.Reactions = out
	if !remove {
		m.Reactions = append(m.Reactions, Reaction{Emoji: emoji, By: who, TS: ts})
	}
	a.updateMsg(ctx, m)
	if !remove && m.FromMe && by != a.myACI() {
		a.bus.Publish(Event{Type: "reaction", Chat: m.Chat, Msg: m, Text: emoji + "\x00" + a.names.nameOf(by)})
	}
}

func (a *App) applyDelete(ctx context.Context, chat, target string) {
	m := a.findMsg(ctx, chat, target)
	if m == nil {
		return
	}
	m.Kind, m.Text, m.Media, m.Reply, m.Reactions = "deleted", "", nil, nil, nil
	_ = a.st.PutMsg(ctx, m, []byte{})
	a.updateMsg(ctx, m)
}

func (a *App) handleEdit(ctx context.Context, chat string, sender uuid.UUID, em *signalpb.EditMessage) {
	m := a.findMsg(ctx, chat, msgID(sender, em.GetTargetSentTimestamp()))
	if m == nil || m.Kind == "deleted" {
		return
	}
	nm, _ := a.convertDataMessage(m.Chat, sender, em.GetDataMessage(), nil)
	if nm == nil {
		return
	}
	m.Text = nm.Text
	m.Edited = true
	m.Mentions = nm.Mentions
	a.updateMsg(ctx, m)
}

func (a *App) handleReceipt(ctx context.Context, e *events.Receipt) {
	var status string
	switch e.Content.GetType() {
	case signalpb.ReceiptMessage_DELIVERY:
		status = "delivered"
	case signalpb.ReceiptMessage_READ, signalpb.ReceiptMessage_VIEWED:
		status = "read"
	default:
		return
	}
	me := a.myACI()
	for _, ts := range e.Content.GetTimestamp() {
		m := a.findMsg(ctx, "", msgID(me, ts))
		if m == nil || !m.FromMe || statusRank(m.Status) >= statusRank(status) {
			continue
		}
		m.Status = status
		_ = a.st.PutMsg(ctx, m, nil)
		a.names.fill(m)
		a.bus.Publish(Event{Type: "msg", Chat: m.Chat, Msg: m})
	}
}

func statusRank(s string) int {
	switch s {
	case "pending":
		return 1
	case "sent":
		return 2
	case "delivered":
		return 3
	case "read":
		return 4
	}
	return 0
}

// handleReadSelf: messages were read on another of the user's devices.
func (a *App) handleReadSelf(ctx context.Context, e *events.ReadSelf) {
	latest := map[string]int64{}
	for _, r := range e.Messages {
		author, err := signalmeow.ParseStringOrBinaryUUID(r.GetSenderAci(), r.GetSenderAciBinary())
		if err != nil {
			continue
		}
		m := a.findMsg(ctx, "", msgID(author, r.GetTimestamp()))
		if m == nil {
			continue
		}
		if m.TS > latest[m.Chat] {
			latest[m.Chat] = m.TS
		}
	}
	for chat, ts := range latest {
		left, _ := a.st.IncomingSince(ctx, chat, ts)
		read, _ := a.st.IncomingSince(ctx, chat, 0)
		a.startExpiry(ctx, read, ts)
		a.st.SetUnread(ctx, chat, len(left))
		a.bus.Publish(Event{Type: "chat", Chat: chat})
	}
}

func (a *App) handleDeleteForMe(ctx context.Context, e *events.DeleteForMe) {
	for _, md := range e.GetMessageDeletes() {
		for _, am := range md.GetMessages() {
			author, err := signalmeow.ParseStringOrBinaryUUID(am.GetAuthorServiceId(), am.GetAuthorServiceIdBinary())
			if err != nil {
				continue
			}
			if m := a.findMsg(ctx, "", msgID(author, am.GetSentTimestamp())); m != nil {
				a.st.DeleteMsg(ctx, m.Chat, m.ID)
				a.st.RefreshPreview(ctx, m.Chat)
				a.bus.Publish(Event{Type: "remove", Chat: m.Chat, Msg: m})
			}
		}
	}
}

// startExpiry starts disappearing-message timers for read messages up to ts.
func (a *App) startExpiry(ctx context.Context, msgs []*Msg, upTo int64) {
	now := time.Now().UnixMilli()
	for _, m := range msgs {
		if m.ExpiresIn > 0 && m.ExpiresAt == 0 && (upTo == 0 || m.TS <= upTo) {
			m.ExpiresAt = now + int64(m.ExpiresIn)*1000
			_ = a.st.PutMsg(ctx, m, nil)
		}
	}
}

func (a *App) expiryLoop() {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-t.C:
			gone := a.st.Expired(a.ctx)
			chats := map[string]bool{}
			for _, m := range gone {
				a.bus.Publish(Event{Type: "remove", Chat: m.Chat, Msg: m})
				chats[m.Chat] = true
			}
			for c := range chats {
				a.st.RefreshPreview(a.ctx, c)
				a.bus.Publish(Event{Type: "chat", Chat: c})
			}
		}
	}
}

// ---------------------------------------------------------------- reading

func (a *App) Chats(ctx context.Context, archived, names bool) ([]*Chat, error) {
	chats, err := a.st.ListChats(ctx, archived)
	if err != nil {
		return nil, err
	}
	for _, c := range chats {
		a.decorateChat(c, names)
	}
	if chats == nil {
		chats = []*Chat{}
	}
	return chats, nil
}

func (a *App) decorateChat(c *Chat, names bool) {
	if c.Kind == "self" {
		c.Name = "Note to Self"
	} else if c.Name == "" {
		c.Name = a.names.chatFallback(c.ID)
	}
	if c.Kind == "dm" {
		if u, err := uuid.Parse(c.ID); err == nil {
			c.Phone = a.names.phoneOf(u)
		}
	}
	if c.Kind == "group" && c.PreviewFrom != "" && !c.PreviewMine && !strings.HasPrefix(c.Preview, "Disappearing messages") &&
		!strings.Contains(c.Preview, " updated the group") && !strings.Contains(c.Preview, " started a group call") {
		if u, err := uuid.Parse(c.PreviewFrom); err == nil {
			c.PreviewFrom = a.names.shortName(u)
		}
	} else {
		c.PreviewFrom = ""
	}
	if names {
		c.Preview = emojiToNames(c.Preview)
		c.Name = emojiToNames(c.Name)
	}
}

func (a *App) ChatInfo(ctx context.Context, id string, names bool) (*Chat, error) {
	c, err := a.st.GetChat(ctx, id)
	if err != nil {
		return nil, err
	} else if c == nil {
		return nil, errors.New("no such chat")
	}
	a.decorateChat(c, names)
	return c, nil
}

// OpenChat makes sure a chat with a contact exists (for "New chat").
func (a *App) OpenChat(ctx context.Context, chat string, names bool) (*Chat, error) {
	if chat == "" {
		return nil, errors.New("no chat")
	}
	if !isGroupChat(chat) {
		if _, err := uuid.Parse(chat); err != nil {
			return nil, errors.New("bad chat id")
		}
	}
	kind := a.chatKind(chat)
	if err := a.st.EnsureChat(ctx, chat, kind); err != nil {
		return nil, err
	}
	a.names.ensureChatName(chat, kind)
	return a.ChatInfo(ctx, chat, names)
}

func (a *App) Messages(ctx context.Context, chat string, limit int, beforeTS int64, beforeID string, names bool) ([]*Msg, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 40
	}
	msgs, more, err := a.st.ListMsgs(ctx, chat, limit, beforeTS, beforeID)
	if err != nil {
		return nil, false, err
	}
	// oldest first
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	for i, m := range msgs {
		a.names.fill(m)
		if names {
			msgs[i] = m.withEmojiNames()
		}
	}
	if msgs == nil {
		msgs = []*Msg{}
	}
	return msgs, more, nil
}

type Contact struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Phone string `json:"phone,omitempty"`
}

func (a *App) Contacts(ctx context.Context) ([]Contact, error) {
	cli := a.client()
	if cli == nil {
		return demoContacts(a), nil
	}
	all, err := cli.Store.RecipientStore.LoadAllContacts(ctx)
	if err != nil {
		return nil, err
	}
	out := []Contact{}
	for _, r := range all {
		if r.ACI == uuid.Nil || r.Blocked || r.ACI == cli.Store.ACI {
			continue
		}
		if r.ContactName == "" && r.Nickname == "" && r.Profile.Name == "" {
			continue // someone we only know by number: use "Find by phone number"
		}
		out = append(out, Contact{ID: r.ACI.String(), Name: displayName(r), Phone: r.E164})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// Lookup finds a Signal account by phone number or username and opens a chat.
func (a *App) Lookup(ctx context.Context, phone, username string) (any, error) {
	cli := a.client()
	if cli == nil {
		return nil, errors.New("not linked")
	}
	var aci uuid.UUID
	if username != "" {
		sid, err := cli.ResolveUsername(ctx, strings.TrimPrefix(strings.TrimSpace(username), "@"))
		if err != nil {
			return nil, fmt.Errorf("username lookup failed: %w", err)
		}
		if sid.Type != libsignalgo.ServiceIDTypeACI || sid.UUID == uuid.Nil {
			return nil, httpErr{404, "No Signal account has that username."}
		}
		aci = sid.UUID
	} else {
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, phone)
		if strings.HasPrefix(strings.TrimSpace(phone), "00") {
			digits = strings.TrimPrefix(digits, "00")
		}
		if len(digits) < 7 {
			return nil, httpErr{400, "Enter the number with its country code, e.g. +351 912 345 678."}
		}
		n, _ := strconv.ParseUint(digits, 10, 64)
		resp, err := cli.LookupPhone(ctx, n)
		if err != nil {
			return nil, fmt.Errorf("lookup failed: %w", err)
		}
		e, ok := resp[n]
		if !ok || e.ACI == uuid.Nil {
			return nil, httpErr{404, "That number isn't on Signal (or hides itself from lookups)."}
		}
		aci = e.ACI
		_, _ = cli.Store.RecipientStore.LoadAndUpdateRecipient(ctx, e.ACI, e.PNI, func(r *types.Recipient) (bool, error) {
			if r.E164 == "" {
				r.E164 = "+" + digits
				return true, nil
			}
			return false, nil
		})
	}
	chat := aci.String()
	_ = a.st.EnsureChat(ctx, chat, a.chatKind(chat))
	a.names.ensureChatName(chat, "dm")
	return map[string]string{"chat": chat, "name": a.names.nameOf(aci)}, nil
}

// ---------------------------------------------------------------- sending

func (a *App) sendLoop() {
	for {
		select {
		case <-a.ctx.Done():
			return
		case fn := <-a.sendQueue:
			fn()
		}
	}
}

func (a *App) enqueue(fn func()) error {
	select {
	case a.sendQueue <- fn:
		return nil
	default:
		return errors.New("too many messages waiting to be sent")
	}
}

// deliver sends content to a chat and reports whether it went out.
func (a *App) deliver(ctx context.Context, chat string, content *signalpb.Content) error {
	cli := a.client()
	if cli == nil {
		return errors.New("not linked")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if isGroupChat(chat) {
		res, err := cli.SendGroupMessage(ctx, types.GroupIdentifier(chat), content)
		if err != nil {
			return err
		}
		if len(res.SuccessfullySentTo) == 0 && len(res.FailedToSendTo) > 0 {
			return res.FailureError()
		}
		return nil
	}
	sid, err := libsignalgo.ServiceIDFromString(chat)
	if err != nil {
		return err
	}
	res := cli.SendMessage(ctx, sid, content)
	if !res.WasSuccessful {
		if res.Error != nil {
			return res.Error
		}
		return errors.New("not delivered")
	}
	return nil
}

func (a *App) baseDataMessage(ctx context.Context, chat string, ts uint64) *signalpb.DataMessage {
	dm := &signalpb.DataMessage{Timestamp: proto.Uint64(ts)}
	if timer, ver := a.st.ChatTimer(ctx, chat); timer > 0 || ver > 0 {
		dm.ExpireTimer = proto.Uint32(uint32(timer))
		if ver > 0 {
			dm.ExpireTimerVersion = proto.Uint32(uint32(ver))
		}
	}
	if cli := a.client(); cli != nil {
		if pk, err := cli.Store.RecipientStore.MyProfileKey(ctx); err == nil && pk != nil {
			dm.ProfileKey = pk.Slice()
		}
	}
	return dm
}

func (a *App) Send(chat, text, replyTo, retryID string, names bool) (*Msg, error) {
	ctx := a.ctx
	if retryID != "" {
		if old := a.findMsg(ctx, chat, retryID); old != nil && old.FromMe && old.Status == "failed" {
			a.st.DeleteMsg(ctx, chat, old.ID)
			a.bus.Publish(Event{Type: "remove", Chat: chat, Msg: old})
			if replyTo == "" && old.Reply != nil {
				replyTo = old.Reply.ID
			}
		}
	}
	if a.demo {
		return demoSend(a, chat, text, replyTo)
	}
	cli := a.client()
	if cli == nil {
		return nil, errors.New("not linked")
	}
	text = strings.TrimRight(namesToEmoji(text), " \n")
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("empty message")
	}
	c, _ := a.st.GetChat(ctx, chat)
	if c == nil {
		return nil, errors.New("no such chat")
	}
	if c.ReadOnly {
		return nil, httpErr{403, "Only admins can send messages to this group."}
	}
	me := cli.Store.ACI
	var m *Msg
	ts := a.nextTS()
	m = &Msg{ID: msgID(me, ts), Chat: chat, TS: int64(ts), Sender: me.String(), FromMe: true, Kind: "text",
		Text: text, Status: "pending"}
	dm := a.baseDataMessage(ctx, chat, ts)
	dm.Body = proto.String(text)
	if dm.GetExpireTimer() > 0 {
		m.ExpiresIn = int(dm.GetExpireTimer())
		m.ExpiresAt = m.TS + int64(m.ExpiresIn)*1000
	}
	if replyTo != "" {
		if q := a.findMsg(ctx, chat, replyTo); q != nil {
			author, qts, err := parseMsgID(q.ID)
			if err == nil {
				dm.Quote = &signalpb.DataMessage_Quote{
					Id:              proto.Uint64(qts),
					AuthorAciBinary: author[:],
					Text:            proto.String(q.Text),
					Type:            signalpb.DataMessage_Quote_NORMAL.Enum(),
				}
				for _, md := range q.Media {
					dm.Quote.Attachments = append(dm.Quote.Attachments, &signalpb.DataMessage_Quote_QuotedAttachment{
						ContentType: proto.String(md.Mime), FileName: nonEmpty(md.Name),
					})
				}
				m.Reply = &Reply{ID: q.ID, Sender: q.Sender, Text: q.Preview()}
			}
		}
	}
	a.storeIncoming(ctx, m, nil, false)
	a.st.SetUnread(ctx, chat, 0)
	sent := *m
	err := a.enqueue(func() {
		err := a.deliver(ctx, chat, signalmeow.WrapDataMessage(dm))
		cur := a.findMsg(ctx, chat, sent.ID)
		if cur == nil {
			return
		}
		if err != nil {
			a.log.Warn().Err(err).Str("chat", chat).Msg("Send failed")
			cur.Status, cur.Error = "failed", friendlySendError(err)
		} else if statusRank(cur.Status) < statusRank("sent") {
			cur.Status, cur.Error = "sent", ""
		}
		_ = a.st.PutMsg(ctx, cur, nil)
		a.names.fill(cur)
		a.bus.Publish(Event{Type: "msg", Chat: chat, Msg: cur})
	})
	if err != nil {
		return nil, err
	}
	a.names.fill(&sent)
	if names {
		return sent.withEmojiNames(), nil
	}
	return &sent, nil
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func friendlySendError(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "context deadline"), strings.Contains(s, "timeout"):
		return "Timed out"
	case strings.Contains(s, "not connected"), strings.Contains(s, "websocket"):
		return "Not connected"
	case strings.Contains(s, "unregistered"), strings.Contains(s, "404"):
		return "This person is no longer on Signal"
	}
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}

func (a *App) Edit(chat, id, text string) error {
	if a.demo {
		return demoEdit(a, chat, id, text)
	}
	ctx := a.ctx
	m := a.findMsg(ctx, chat, id)
	if m == nil || !m.FromMe || m.Kind == "deleted" {
		return errors.New("can't edit this message")
	}
	if time.Since(time.UnixMilli(m.TS)) > 24*time.Hour {
		return httpErr{400, "Signal only allows editing messages for 24 hours."}
	}
	text = strings.TrimSpace(namesToEmoji(text))
	if text == "" {
		return errors.New("empty message")
	}
	_, origTS, err := parseMsgID(m.ID)
	if err != nil {
		return err
	}
	dm := a.baseDataMessage(ctx, chat, a.nextTS())
	dm.Body = proto.String(text)
	m.Text, m.Edited = text, true
	a.updateMsg(ctx, m)
	return a.enqueue(func() {
		if err := a.deliver(ctx, chat, signalmeow.WrapEditMessage(&signalpb.EditMessage{
			TargetSentTimestamp: proto.Uint64(origTS), DataMessage: dm,
		})); err != nil {
			a.bus.Publish(Event{Type: "error", Chat: chat, Text: "Edit not sent: " + friendlySendError(err)})
		}
	})
}

func (a *App) Delete(chat, id string, forEveryone bool) error {
	ctx := a.ctx
	m := a.findMsg(ctx, chat, id)
	if m == nil {
		return errors.New("no such message")
	}
	if !forEveryone || a.demo || m.Status == "failed" || m.Status == "pending" {
		a.st.DeleteMsg(ctx, m.Chat, m.ID)
		a.st.RefreshPreview(ctx, m.Chat)
		a.bus.Publish(Event{Type: "remove", Chat: m.Chat, Msg: m})
		a.bus.Publish(Event{Type: "chat", Chat: m.Chat})
		return nil
	}
	if !m.FromMe {
		return errors.New("you can only delete your own messages for everyone")
	}
	_, origTS, err := parseMsgID(m.ID)
	if err != nil {
		return err
	}
	dm := a.baseDataMessage(ctx, chat, a.nextTS())
	dm.ProfileKey = nil
	dm.Delete = &signalpb.DataMessage_Delete{TargetSentTimestamp: proto.Uint64(origTS)}
	a.applyDelete(ctx, chat, m.ID)
	return a.enqueue(func() {
		if err := a.deliver(ctx, chat, signalmeow.WrapDataMessage(dm)); err != nil {
			a.bus.Publish(Event{Type: "error", Chat: chat, Text: "Delete not sent: " + friendlySendError(err)})
		}
	})
}

func (a *App) React(chat, id, emoji string) error {
	ctx := a.ctx
	emoji = namesToEmoji(strings.TrimSpace(emoji))
	m := a.findMsg(ctx, chat, id)
	if m == nil || m.Kind == "deleted" {
		return errors.New("no such message")
	}
	me := a.myACI()
	if a.demo {
		me = demoMe
	}
	remove := false
	for _, r := range m.Reactions {
		if r.By == me.String() && (r.Emoji == emoji || emoji == "") {
			remove, emoji = true, r.Emoji
		}
	}
	if emoji == "" {
		return nil
	}
	a.applyReaction(ctx, chat, m.ID, me, emoji, remove, time.Now().UnixMilli())
	if a.demo {
		return nil
	}
	author, origTS, err := parseMsgID(m.ID)
	if err != nil {
		return err
	}
	dm := a.baseDataMessage(ctx, chat, a.nextTS())
	dm.Reaction = &signalpb.DataMessage_Reaction{
		Emoji: proto.String(emoji), Remove: proto.Bool(remove),
		TargetAuthorAciBinary: author[:], TargetSentTimestamp: proto.Uint64(origTS),
	}
	return a.enqueue(func() {
		if err := a.deliver(ctx, chat, signalmeow.WrapDataMessage(dm)); err != nil {
			a.bus.Publish(Event{Type: "error", Chat: chat, Text: "Reaction not sent: " + friendlySendError(err)})
		}
	})
}

// MarkRead clears the unread count and tells the senders (if read receipts
// are on) and the user's other devices.
func (a *App) MarkRead(chat string) error {
	ctx := a.ctx
	c, _ := a.st.GetChat(ctx, chat)
	if c == nil {
		return errors.New("no such chat")
	}
	if c.Unread == 0 {
		return nil
	}
	a.st.SetUnread(ctx, chat, 0)
	a.bus.Publish(Event{Type: "chat", Chat: chat})
	recent, _, _ := a.st.ListMsgs(ctx, chat, max(c.Unread*2, 20), 0, "")
	var unread []*Msg
	for _, m := range recent {
		if !m.FromMe && m.Kind != "system" && len(unread) < c.Unread {
			unread = append(unread, m)
		}
	}
	a.startExpiry(ctx, unread, 0)
	cli := a.client()
	if cli == nil || len(unread) == 0 {
		return nil
	}
	bySender := map[uuid.UUID][]uint64{}
	var syncReads []*signalpb.SyncMessage_Read
	for _, m := range unread {
		author, ts, err := parseMsgID(m.ID)
		if err != nil {
			continue
		}
		bySender[author] = append(bySender[author], ts)
		syncReads = append(syncReads, &signalpb.SyncMessage_Read{SenderAciBinary: author[:], Timestamp: proto.Uint64(ts)})
	}
	receiptsOn := cli.Store.AccountRecord == nil || cli.Store.AccountRecord.GetReadReceipts()
	return a.enqueue(func() {
		if receiptsOn {
			for sender, tss := range bySender {
				sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				cli.SendMessage(sctx, libsignalgo.NewACIServiceID(sender), signalmeow.ReadReceptMessageForTimestamps(tss))
				cancel()
			}
			return
		}
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		cli.SendMessage(sctx, cli.Store.ACIServiceID(), signalmeow.WrapSyncMessage(&signalpb.SyncMessage{Read: syncReads}))
		cancel()
	})
}

func (a *App) SetChatFlags(ctx context.Context, b reqBody) error {
	chat := b.str("chat")
	for _, k := range []string{"pinned", "muted", "archived"} {
		if b.has(k) {
			if err := a.st.SetChatFlag(ctx, chat, k, b.boolean(k)); err != nil {
				return err
			}
		}
	}
	a.bus.Publish(Event{Type: "chat", Chat: chat})
	return nil
}

func (a *App) ClearChat(ctx context.Context, chat string) error {
	if err := a.st.ClearChat(ctx, chat); err != nil {
		return err
	}
	a.bus.Publish(Event{Type: "chat", Chat: chat, Reload: true})
	return nil
}

func fmtDuration(secs int) string {
	switch {
	case secs%(7*86400) == 0:
		return plural(secs/(7*86400), "week")
	case secs%86400 == 0:
		return plural(secs/86400, "day")
	case secs%3600 == 0:
		return plural(secs/3600, "hour")
	case secs%60 == 0:
		return plural(secs/60, "minute")
	}
	return plural(secs, "second")
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}
