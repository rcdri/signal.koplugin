package main

import (
	"testing"

	"github.com/google/uuid"
)

func TestEmojiNames(t *testing.T) {
	in := "Hi 👍 ❤️ 👨‍👩‍👧 🇵🇹 1️⃣ ©"
	names := emojiToNames(in)
	if names != "Hi :thumbsup: :heart: :family_man_woman_girl: :flag_for_portugal: :one: ©" {
		t.Fatalf("emojiToNames: %q", names)
	}
	if back := namesToEmoji(":thumbsup: :heart: :nonexistent: 10:30"); back != "👍 ❤️ :nonexistent: 10:30" {
		t.Fatalf("namesToEmoji: %q", back)
	}
}

func TestIDs(t *testing.T) {
	u := uuid.New()
	id := msgID(u, 1700000000123)
	pu, ts, err := parseMsgID(id)
	if err != nil || pu != u || ts != 1700000000123 {
		t.Fatalf("parseMsgID(%q) = %v %v %v", id, pu, ts, err)
	}
	if isGroupChat(u.String()) || !isGroupChat("ZGVtby1ib29rLWNsdWItZ3JvdXAtaWRlbnRpZmllcjAxMjM0NQ==") || isGroupChat("PNI:"+u.String()) {
		t.Fatal("isGroupChat")
	}
}

func TestPreview(t *testing.T) {
	m := &Msg{Kind: "media", Text: "look", Media: []Media{{Kind: "image"}}}
	if p := m.Preview(); p != "[Photo] look" {
		t.Fatal(p)
	}
	if p := (&Msg{Kind: "deleted"}).Preview(); p != "This message was deleted" {
		t.Fatal(p)
	}
	if fmtDuration(604800) != "1 week" || fmtDuration(3600*8) != "8 hours" || fmtDuration(30) != "30 seconds" {
		t.Fatal("fmtDuration")
	}
}
