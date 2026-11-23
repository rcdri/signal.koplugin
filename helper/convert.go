package main

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
)

const longTextType = "text/x-signal-plain"

type storedAtt struct {
	P []byte `json:"p"`
	H []byte `json:"h,omitempty"` // plaintext hash, for attachments from a history transfer
}

func mediaKind(att *signalpb.AttachmentPointer) string {
	ct := strings.ToLower(att.GetContentType())
	switch {
	case att.GetFlags()&uint32(signalpb.AttachmentPointer_VOICE_MESSAGE) != 0:
		return "voice"
	case att.GetFlags()&uint32(signalpb.AttachmentPointer_GIF) != 0, ct == "image/gif":
		return "gif"
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.HasPrefix(ct, "video/"):
		return "video"
	case strings.HasPrefix(ct, "audio/"):
		return "audio"
	}
	return "file"
}

// applyMentions replaces mention placeholders (U+FFFC) with "@Name".
func (a *App) applyMentions(body string, ranges []*signalpb.BodyRange) (string, bool) {
	type mention struct {
		start, length int
		who           uuid.UUID
	}
	var ms []mention
	for _, r := range ranges {
		var who uuid.UUID
		var err error
		switch v := r.AssociatedValue.(type) {
		case *signalpb.BodyRange_MentionAci:
			who, err = uuid.Parse(v.MentionAci)
		case *signalpb.BodyRange_MentionAciBinary:
			who, err = uuid.FromBytes(v.MentionAciBinary)
		default:
			continue
		}
		if err == nil {
			ms = append(ms, mention{int(r.GetStart()), int(r.GetLength()), who})
		}
	}
	if len(ms) == 0 {
		return body, false
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].start > ms[j].start })
	u := utf16.Encode([]rune(body))
	me := a.myACI()
	mine := false
	for _, m := range ms {
		if m.start < 0 || m.length < 0 || m.start+m.length > len(u) {
			continue
		}
		if m.who == me {
			mine = true
		}
		name := utf16.Encode([]rune("@" + a.names.nameOf(m.who)))
		u = append(u[:m.start], append(name, u[m.start+m.length:]...)...)
	}
	return string(utf16.Decode(u)), mine
}

// convertDataMessage turns a Signal message into what the plugin shows.
// hashes holds plaintext hashes for attachments (history transfer only).
func (a *App) convertDataMessage(chat string, sender uuid.UUID, dm *signalpb.DataMessage, hashes map[int][]byte) (*Msg, []byte) {
	if dm == nil {
		return nil, nil
	}
	ts := dm.GetTimestamp()
	me := a.myACI()
	m := &Msg{ID: msgID(sender, ts), Chat: chat, TS: int64(ts), Sender: sender.String(), FromMe: sender == me && me != uuid.Nil, Kind: "text"}
	m.Text, m.Mentions = a.applyMentions(dm.GetBody(), dm.GetBodyRanges())

	if q := dm.GetQuote(); q != nil {
		author, err := signalmeow.ParseStringOrBinaryUUID(q.GetAuthorAci(), q.GetAuthorAciBinary())
		if err == nil {
			r := &Reply{ID: msgID(author, q.GetId()), Sender: author.String()}
			r.Text, _ = a.applyMentions(q.GetText(), q.GetBodyRanges())
			if orig, _ := a.st.GetMsg(a.ctx, chat, r.ID); orig != nil {
				r.Text = orig.Preview()
			} else if r.Text == "" && len(q.GetAttachments()) > 0 {
				r.Text = "[Attachment]"
				ct := q.GetAttachments()[0].GetContentType()
				if strings.HasPrefix(ct, "image/") {
					r.Text = "[Photo]"
				} else if strings.HasPrefix(ct, "video/") {
					r.Text = "[Video]"
				}
			}
			m.Reply = r
		}
	}
	if sc := dm.GetStoryContext(); sc != nil && m.Reply == nil {
		m.Reply = &Reply{ID: "", Text: "Story"}
	}

	var stored []storedAtt
	addAtt := func(p *signalpb.AttachmentPointer, idx int) {
		b, err := proto.Marshal(p)
		if err != nil {
			return
		}
		stored = append(stored, storedAtt{P: b, H: hashes[idx]})
	}
	if dm.GetIsViewOnce() {
		m.ViewOnce = true
		m.Kind = "system"
		m.Text = "View-once media (open it on your phone)"
		return m, nil
	}
	for i, att := range dm.GetAttachments() {
		if att.GetContentType() == longTextType {
			continue // fetched separately, replaces the body
		}
		m.Media = append(m.Media, Media{
			Kind: mediaKind(att), Mime: att.GetContentType(), Name: att.GetFileName(), Size: int64(att.GetSize()),
			Width: int(att.GetWidth()), Height: int(att.GetHeight()), Secs: int(att.GetAudioDurationSeconds() + 0.5),
		})
		addAtt(att, i)
		if c := att.GetCaption(); c != "" && m.Text == "" {
			m.Text = c
		}
	}
	if st := dm.GetSticker(); st != nil {
		m.Kind = "sticker"
		m.Text = st.GetEmoji()
		if st.GetData() != nil {
			m.Media = []Media{{Kind: "sticker", Mime: st.GetData().GetContentType()}}
			stored = nil
			addAtt(st.GetData(), 0)
		}
	}
	if len(dm.GetContact()) > 0 {
		m.Kind = "contact"
		var parts []string
		for _, c := range dm.GetContact() {
			name := strings.TrimSpace(c.GetName().GetGivenName() + " " + c.GetName().GetFamilyName())
			if name == "" {
				name = c.GetName().GetNickname()
			}
			line := "👤 " + name
			for _, p := range c.GetNumber() {
				line += "\n" + p.GetValue()
			}
			parts = append(parts, line)
		}
		m.Text = strings.Join(parts, "\n\n")
	}
	if p := dm.GetPollCreate(); p != nil {
		m.Kind = "poll"
		lines := []string{"📊 " + p.GetQuestion()}
		for _, o := range p.GetOptions() {
			lines = append(lines, "• "+o)
		}
		lines = append(lines, "(vote on your phone)")
		m.Text = strings.Join(lines, "\n")
	}
	if dm.GetPayment() != nil {
		m.Kind, m.Text = "system", "Payment (see your phone)"
	}
	if dm.GetGiftBadge() != nil {
		m.Kind, m.Text = "system", "Gift badge (see your phone)"
	}
	if len(m.Media) > 0 && m.Kind == "text" {
		m.Kind = "media"
	}
	if m.Kind == "text" && strings.TrimSpace(m.Text) == "" {
		hasLong := false
		for _, att := range dm.GetAttachments() {
			if att.GetContentType() == longTextType {
				hasLong = true
			}
		}
		if !hasLong {
			return nil, nil
		}
		m.Text = "…"
	}
	var attsJSON []byte
	if len(stored) > 0 {
		attsJSON, _ = json.Marshal(stored)
	}
	return m, attsJSON
}

// maybeFetchLongText downloads the full text of long messages (Signal sends
// the first 2 KB in the body and the rest as an attachment).
func (a *App) maybeFetchLongText(chat, id string, dm *signalpb.DataMessage) {
	var ptr *signalpb.AttachmentPointer
	for _, att := range dm.GetAttachments() {
		if att.GetContentType() == longTextType {
			ptr = att
		}
	}
	if ptr == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(a.ctx, 60*time.Second)
		defer cancel()
		data, err := signalmeow.DownloadAttachmentWithPointer(ctx, ptr, nil, nil)
		if err != nil {
			a.log.Warn().Err(err).Msg("Long text download failed")
			return
		}
		if len(data) == 0 {
			return
		}
		if len(data) > 256<<10 {
			data = data[:256<<10]
		}
		m := a.findMsg(a.ctx, chat, id)
		if m == nil {
			return
		}
		m.Text = string(data)
		a.updateMsg(a.ctx, m)
	}()
}
