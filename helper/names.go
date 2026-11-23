package main

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"go.mau.fi/mautrix-signal/pkg/signalmeow/types"
)

// Names resolves people's and groups' display names, fetching Signal
// profiles in the background when the contact list doesn't have them.
type Names struct {
	a        *App
	mu       sync.Mutex
	cache    map[uuid.UUID]nameEntry
	queued   map[uuid.UUID]bool
	groupsQ  map[string]bool
	fetchQ   chan uuid.UUID
	groupQ   chan string
	announce chan struct{}
}

type nameEntry struct {
	name, phone string
	at          time.Time
}

func NewNames(a *App) *Names {
	n := &Names{
		a:        a,
		cache:    map[uuid.UUID]nameEntry{},
		queued:   map[uuid.UUID]bool{},
		groupsQ:  map[string]bool{},
		fetchQ:   make(chan uuid.UUID, 512),
		groupQ:   make(chan string, 256),
		announce: make(chan struct{}, 1),
	}
	go n.profileWorker()
	go n.groupWorker()
	go n.announcer()
	return n
}

func (n *Names) reset() {
	n.mu.Lock()
	n.cache = map[uuid.UUID]nameEntry{}
	n.queued = map[uuid.UUID]bool{}
	n.groupsQ = map[string]bool{}
	n.mu.Unlock()
}

func displayName(r *types.Recipient) string {
	for _, s := range []string{r.Nickname, r.ContactName, r.Profile.Name} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return r.E164
}

func (n *Names) lookup(u uuid.UUID) (nameEntry, bool) {
	n.mu.Lock()
	e, ok := n.cache[u]
	n.mu.Unlock()
	if ok {
		return e, true
	}
	cli := n.a.client()
	if cli == nil {
		if d, ok := demoNames[u]; ok {
			return nameEntry{name: d}, true
		}
		return nameEntry{}, false
	}
	r, err := cli.Store.RecipientStore.LoadAndUpdateRecipient(n.a.ctx, u, uuid.Nil, nil)
	if err != nil || r == nil {
		return nameEntry{}, false
	}
	e = nameEntry{name: displayName(r), phone: r.E164, at: time.Now()}
	if r.ContactName == "" && r.Profile.Name == "" && r.Nickname == "" {
		n.queueProfile(u)
		if e.name == "" {
			return e, false
		}
	}
	n.mu.Lock()
	n.cache[u] = e
	n.mu.Unlock()
	return e, true
}

func (n *Names) queueProfile(u uuid.UUID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.queued[u] {
		return
	}
	n.queued[u] = true
	select {
	case n.fetchQ <- u:
	default:
		delete(n.queued, u)
	}
}

func (n *Names) nameOf(u uuid.UUID) string {
	if u == n.a.myACI() && u != uuid.Nil {
		return "You"
	}
	if e, ok := n.lookup(u); ok && e.name != "" {
		return e.name
	}
	return "Signal user"
}

func (n *Names) cachedName(u uuid.UUID) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.cache[u].name
}

func (n *Names) phoneOf(u uuid.UUID) string {
	e, _ := n.lookup(u)
	return e.phone
}

func (n *Names) shortName(u uuid.UUID) string {
	name := n.nameOf(u)
	if strings.HasPrefix(name, "+") {
		return name
	}
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	return name
}

func (n *Names) chatFallback(chat string) string {
	if isGroupChat(chat) {
		n.queueGroup(chat)
		return "Group"
	}
	if u, err := uuid.Parse(chat); err == nil {
		return n.nameOf(u)
	}
	return "Signal user"
}

// fill adds display names to a message (they aren't stored).
func (n *Names) fill(m *Msg) {
	if !m.FromMe && m.Kind != "system" {
		if u, err := uuid.Parse(m.Sender); err == nil {
			m.SenderName = n.nameOf(u)
		}
	}
	if m.Reply != nil {
		if u, err := uuid.Parse(m.Reply.Sender); err == nil {
			m.Reply.SenderName = n.nameOf(u)
		}
	}
	for i := range m.Reactions {
		if u, err := uuid.Parse(m.Reactions[i].By); err == nil {
			m.Reactions[i].Name = n.nameOf(u)
		}
	}
}

func (n *Names) ensureChatName(chat, kind string) {
	switch kind {
	case "group":
		if c, _ := n.a.st.GetChat(n.a.ctx, chat); c != nil && c.Name == "" {
			n.queueGroup(chat)
		}
	case "dm":
		if u, err := uuid.Parse(chat); err == nil {
			if e, ok := n.lookup(u); ok && e.name != "" {
				n.a.st.SetChatName(n.a.ctx, chat, e.name)
			}
		}
	}
}

func (n *Names) contactsChanged(rs []*types.Recipient) {
	changed := false
	for _, r := range rs {
		if r == nil || r.ACI == uuid.Nil {
			continue
		}
		name := displayName(r)
		n.mu.Lock()
		old := n.cache[r.ACI]
		n.cache[r.ACI] = nameEntry{name: name, phone: r.E164, at: time.Now()}
		n.mu.Unlock()
		if old.name != name || old.phone != r.E164 {
			changed = true
		}
		if name != "" && n.a.st.SetChatName(n.a.ctx, r.ACI.String(), name) {
			changed = true
		}
	}
	if changed {
		n.poke()
	}
}

func (n *Names) poke() {
	select {
	case n.announce <- struct{}{}:
	default:
	}
}

// announcer tells the plugin about name changes at most every 3 seconds.
func (n *Names) announcer() {
	for {
		select {
		case <-n.a.ctx.Done():
			return
		case <-n.announce:
			time.Sleep(3 * time.Second)
			n.a.bus.Publish(Event{Type: "names"})
		}
	}
}

func (n *Names) profileWorker() {
	for {
		select {
		case <-n.a.ctx.Done():
			return
		case u := <-n.fetchQ:
			cli := n.a.client()
			if cli == nil {
				continue
			}
			ctx, cancel := context.WithTimeout(n.a.ctx, 20*time.Second)
			r, err := cli.ContactByACIWithRefreshAfter(ctx, u, 24*time.Hour)
			cancel()
			if err != nil || r == nil {
				n.a.log.Debug().Err(err).Stringer("aci", u).Msg("Profile fetch failed")
				continue
			}
			n.contactsChanged([]*types.Recipient{r})
			time.Sleep(300 * time.Millisecond)
		}
	}
}

func (n *Names) queueGroup(chat string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.groupsQ[chat] {
		return
	}
	n.groupsQ[chat] = true
	select {
	case n.groupQ <- chat:
	default:
		delete(n.groupsQ, chat)
	}
}

func (n *Names) groupWorker() {
	for {
		select {
		case <-n.a.ctx.Done():
			return
		case chat := <-n.groupQ:
			n.refreshGroup(n.a.ctx, chat)
			n.mu.Lock()
			delete(n.groupsQ, chat)
			n.mu.Unlock()
		}
	}
}

func (n *Names) refreshGroup(ctx context.Context, chat string) {
	cli := n.a.client()
	if cli == nil || !isGroupChat(chat) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	g, _, err := cli.RetrieveGroupByID(ctx, types.GroupIdentifier(chat), 0)
	if err != nil || g == nil {
		n.a.log.Debug().Err(err).Str("group", chat).Msg("Group fetch failed")
		return
	}
	readOnly := false
	if g.AnnouncementsOnly {
		readOnly = true
		for _, m := range g.Members {
			if m.ACI == cli.Store.ACI && m.Role == 2 { // ADMINISTRATOR
				readOnly = false
			}
		}
	}
	_ = n.a.st.EnsureChat(ctx, chat, "group")
	n.a.st.SetChatGroupInfo(ctx, chat, strings.TrimSpace(g.Title), len(g.Members), readOnly)
	if g.DisappearingMessagesDuration > 0 {
		if t, _ := n.a.st.ChatTimer(ctx, chat); t == 0 {
			n.a.st.SetChatTimer(ctx, chat, int(g.DisappearingMessagesDuration), 0)
		}
	}
	n.a.bus.Publish(Event{Type: "chat", Chat: chat})
	for _, m := range g.Members {
		if m.ACI != cli.Store.ACI {
			n.lookup(m.ACI)
		}
	}
}

// refreshGroups updates names of groups in the chat list once per connection.
func (n *Names) refreshGroups(ctx context.Context) {
	chats, err := n.a.st.ListChats(ctx, false)
	if err != nil {
		return
	}
	archived, _ := n.a.st.ListChats(ctx, true)
	for _, c := range append(chats, archived...) {
		if c.Kind == "group" {
			n.queueGroup(c.ID)
		}
	}
}
