//go:build demo

package main

// A fake account with sample chats, for testing the plugin without Signal.

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	demoMe    = uuid.MustParse("00000000-0000-4000-8000-000000000001")
	demoAna   = uuid.MustParse("00000000-0000-4000-8000-0000000000a1")
	demoBruno = uuid.MustParse("00000000-0000-4000-8000-0000000000b2")
	demoCarla = uuid.MustParse("00000000-0000-4000-8000-0000000000c3")
	demoDiogo = uuid.MustParse("00000000-0000-4000-8000-0000000000d4")
	demoGroup = "ZGVtby1ib29rLWNsdWItZ3JvdXAtaWRlbnRpZmllcjAxMjM0NQ=="
	demoFam   = "ZGVtby1mYW1pbHktZ3JvdXAtaWRlbnRpZmllci0wMTIzNDU2Nzg="
)

var demoNames = map[uuid.UUID]string{
	demoMe: "You", demoAna: "Ana Sousa", demoBruno: "Bruno Lima", demoCarla: "Carla Mendes", demoDiogo: "Diogo",
}

func startDemo(a *App) bool {
	a.demo = true
	if os.Getenv("SIG_DEMO_LINKED") == "1" {
		demoFill(a)
		a.setState("connected", "")
	} else {
		a.setState("unpaired", "")
	}
	go demoChatter(a)
	return true
}

func demoLogin(a *App) error {
	a.mu.Lock()
	a.state, a.detail = "pairing", ""
	a.qr = "sgnl://linkdevice?uuid=DEMO" + time.Now().Format("150405") + "&pub_key=BQ3%2Fdemo%2Bkey%2Bfor%2Btesting%2Bonly"
	a.qrSince = time.Now()
	a.mu.Unlock()
	a.bus.Publish(Event{Type: "qr"})
	if os.Getenv("SIG_DEMO_SCAN") != "" {
		go func() {
			time.Sleep(4 * time.Second)
			a.setState("linking", "")
			time.Sleep(1 * time.Second)
			demoFill(a)
			a.mu.Lock()
			a.syncing = "history"
			a.mu.Unlock()
			a.setState("connected", "")
			time.Sleep(2 * time.Second)
			a.mu.Lock()
			a.syncing = ""
			a.mu.Unlock()
			a.bus.Publish(Event{Type: "state", Text: "synced"})
			a.bus.Publish(Event{Type: "chats", Reload: true})
		}()
	}
	return nil
}

func demoLogout(a *App) error {
	_ = a.st.Wipe(a.ctx)
	a.setState("unpaired", "")
	a.bus.Publish(Event{Type: "chats", Reload: true})
	return nil
}

func demoContacts(a *App) []Contact {
	return []Contact{
		{ID: demoAna.String(), Name: "Ana Sousa", Phone: "+351 912 345 678"},
		{ID: demoBruno.String(), Name: "Bruno Lima", Phone: "+351 913 222 111"},
		{ID: demoCarla.String(), Name: "Carla Mendes", Phone: "+44 7700 900123"},
		{ID: demoDiogo.String(), Name: "Diogo", Phone: "+351 914 555 000"},
	}
}

func demoFill(a *App) {
	ctx := a.ctx
	if c, _ := a.st.GetChat(ctx, demoAna.String()); c != nil {
		return
	}
	now := time.Now().Add(-3 * time.Hour).UnixMilli()
	t := func(min int) int64 { return now + int64(min)*60000 }
	add := func(chat, kind, name string, msgs []*Msg, unread int) {
		_ = a.st.EnsureChat(ctx, chat, kind)
		if name != "" {
			a.st.SetChatName(ctx, chat, name)
		}
		for _, m := range msgs {
			m.Chat = chat
			if m.Kind == "" {
				m.Kind = "text"
			}
			if m.FromMe {
				m.Sender = demoMe.String()
				if m.Status == "" {
					m.Status = "read"
				}
			}
			u, _ := uuid.Parse(m.Sender)
			m.ID = msgID(u, uint64(m.TS))
			_ = a.st.PutMsg(ctx, m, nil)
		}
		a.st.RefreshPreview(ctx, chat)
		a.st.SetUnread(ctx, chat, unread)
	}
	ana, bruno, carla, diogo := demoAna.String(), demoBruno.String(), demoCarla.String(), demoDiogo.String()
	anaMsgs := []*Msg{
		{TS: t(0), Sender: ana, Text: "Olá! Are we still on for Saturday?"},
		{TS: t(2), FromMe: true, Text: "Yes! 10:00 at the café near the station 😀"},
		{TS: t(3), Sender: ana, Text: "Perfect. I'll bring the book.", Reactions: []Reaction{{Emoji: "👍", By: demoMe.String()}}},
		{TS: t(30), Sender: ana, Kind: "media", Text: "The view from the hike yesterday",
			Media: []Media{{Kind: "image", Mime: "image/png", Name: "hike.png", Width: 1600, Height: 1200, Size: 48000}}},
		{TS: t(31), FromMe: true, Text: "Wow, that's beautiful! Where is it?",
			Reply: &Reply{Sender: ana, Text: "[Photo] The view from the hike yesterday"}},
		{TS: t(40), Sender: ana, Text: "Peneda-Gerês, the trail to the waterfall. You'd love it."},
		{TS: t(41), Sender: ana, Kind: "media", Media: []Media{{Kind: "voice", Mime: "audio/aac", Secs: 14}}},
		{TS: t(170), Sender: ana, Text: "Also, did you finish chapter 12? No spoilers please 🙈"},
	}
	anaMsgs[4].Reply.ID = msgID(demoAna, uint64(t(30)))
	add(ana, "dm", "Ana Sousa", anaMsgs, 2)

	add(demoGroup, "group", "Book club 📚", []*Msg{
		{TS: t(10), Sender: bruno, Text: "Next pick: 'The Left Hand of Darkness'? Vote with a reaction.",
			Reactions: []Reaction{{Emoji: "❤️", By: carla}, {Emoji: "👍", By: demoMe.String()}, {Emoji: "👍", By: ana}}},
		{TS: t(12), Sender: carla, Text: "Yes please, I've been meaning to read it for ages"},
		{TS: t(15), FromMe: true, Text: "I'm in. I have it on my Kindle already."},
		{TS: t(20), Sender: bruno, Kind: "media", Text: "Discussion questions",
			Media: []Media{{Kind: "file", Mime: "text/plain", Name: "questions.txt", Size: 812}}},
		{TS: t(25), Sender: carla, Kind: "deleted"},
		{TS: t(60), Sender: bruno, Text: "@Demo can you book the room for the 14th?", Mentions: true},
		{TS: t(65), Sender: carla, Kind: "poll", Text: "📊 Meeting time?\n• 18:00\n• 19:30\n(vote on your phone)"},
	}, 3)
	_, _ = a.db.Exec(ctx, `UPDATE kp_chat SET members=5 WHERE id=$1`, demoGroup)

	add(bruno, "dm", "Bruno Lima", []*Msg{
		{TS: t(50), Sender: bruno, Text: "Can you send me the address?"},
		{TS: t(52), FromMe: true, Text: "Rua do Souto 12, Braga", Status: "delivered"},
		{TS: t(53), FromMe: true, Text: "It's the blue door", Status: "sent"},
	}, 0)
	add(demoFam, "group", "Family", []*Msg{
		{TS: t(-1500), Sender: diogo, Text: "Dinner on Sunday at grandma's 🍲"},
		{TS: t(-1490), Sender: carla, Kind: "sticker", Text: "😋", Media: []Media{{Kind: "sticker", Mime: "image/webp"}}},
		{TS: t(-1480), Sender: demoMe.String(), FromMe: true, Kind: "system", Text: "Disappearing messages set to 1 week"},
	}, 0)
	_ = a.st.SetChatFlag(ctx, demoFam, "muted", true)
	add(carla, "dm", "Carla Mendes", []*Msg{
		{TS: t(-3000), Sender: carla, Kind: "call", Text: "Incoming call (answer it on your phone)"},
		{TS: t(-2990), Sender: carla, Text: "Call me back when you can"},
	}, 0)
	add(demoMe.String(), "self", "", []*Msg{
		{TS: t(-200), FromMe: true, Text: "Shopping: bread, coffee, batteries for the remote"},
	}, 0)
	_ = a.st.SetChatFlag(ctx, demoMe.String(), "pinned", true)
	add(diogo, "dm", "Diogo", []*Msg{{TS: t(-9000), Sender: diogo, Text: "Old chat"}}, 0)
	_ = a.st.SetChatFlag(ctx, diogo, "archived", true)
}

func demoSend(a *App, chat, text, replyTo string) (*Msg, error) {
	ctx := a.ctx
	text = strings.TrimSpace(namesToEmoji(text))
	if text == "" {
		return nil, errors.New("empty message")
	}
	ts := a.nextTS()
	m := &Msg{ID: msgID(demoMe, ts), Chat: chat, TS: int64(ts), Sender: demoMe.String(), FromMe: true, Kind: "text",
		Text: text, Status: "pending"}
	if replyTo != "" {
		if q := a.findMsg(ctx, chat, replyTo); q != nil {
			m.Reply = &Reply{ID: q.ID, Sender: q.Sender, Text: q.Preview()}
		}
	}
	a.storeIncoming(ctx, m, nil, false)
	sent := *m
	go func() {
		for _, st := range []string{"sent", "delivered", "read"} {
			time.Sleep(1200 * time.Millisecond)
			cur := a.findMsg(ctx, chat, sent.ID)
			if cur == nil {
				return
			}
			if strings.Contains(text, "fail") {
				cur.Status, cur.Error = "failed", "Not connected"
				a.updateMsg(ctx, cur)
				return
			}
			cur.Status = st
			a.updateMsg(ctx, cur)
		}
		if strings.HasSuffix(text, "?") && !isGroupChat(chat) && chat != demoMe.String() {
			time.Sleep(1500 * time.Millisecond)
			who, _ := uuid.Parse(chat)
			rts := a.nextTS()
			a.storeIncoming(ctx, &Msg{ID: msgID(who, rts), Chat: chat, TS: int64(rts), Sender: chat, Kind: "text",
				Text: "Good question! Let me check and get back to you 👍"}, nil, true)
		}
	}()
	a.names.fill(&sent)
	return sent.withEmojiNames(), nil
}

func demoEdit(a *App, chat, id, text string) error {
	m := a.findMsg(a.ctx, chat, id)
	if m == nil || !m.FromMe {
		return errors.New("can't edit this message")
	}
	m.Text, m.Edited = namesToEmoji(text), true
	a.updateMsg(a.ctx, m)
	return nil
}

// demoChatter: a message arrives when the file "demo-incoming" appears in the data folder.
func demoChatter(a *App) {
	trigger := a.dataDir + "/demo-incoming"
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		b, err := os.ReadFile(trigger)
		if err != nil {
			continue
		}
		_ = os.Remove(trigger)
		parts := strings.SplitN(strings.TrimSpace(string(b)), "|", 3)
		chat, sender, text := demoAna.String(), demoAna.String(), "Are you there?"
		if len(parts) == 3 {
			chat, sender, text = parts[0], parts[1], parts[2]
		}
		if chat == "group" {
			chat = demoGroup
		}
		ts := a.nextTS()
		su, _ := uuid.Parse(sender)
		a.storeIncoming(a.ctx, &Msg{ID: msgID(su, ts), Chat: chat, TS: int64(ts), Sender: sender, Kind: "text", Text: text}, nil, true)
	}
}

func demoMedia(m *Msg, idx int, path string) error {
	md := m.Media[idx]
	switch md.Kind {
	case "image", "sticker":
		w, h := 1600, 1200
		if md.Kind == "sticker" {
			w, h = 512, 512
		}
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				sky := uint8(120 + 100*y/h)
				c := color.RGBA{sky, sky, 255, 255}
				if y > h/2+int(float64(h/6)*float64((x*7)%w)/float64(w)) {
					c = color.RGBA{40, uint8(90 + (x*y)%60), 40, 255}
				}
				img.Set(x, y, c)
			}
		}
		var b bytes.Buffer
		_ = png.Encode(&b, img)
		return os.WriteFile(path, b.Bytes(), 0o600)
	case "file":
		return os.WriteFile(path, []byte("Book club — discussion questions\n\n1. How does Genly Ai's view of Gethen change?\n2. What does the journey across the ice reveal?\n3. Favourite quote?\n"), 0o600)
	}
	return errors.New("this demo file can't be opened")
}

var _ = context.Background
