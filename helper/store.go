package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"go.mau.fi/util/dbutil"
)

// ---------------------------------------------------------------- model

type Media struct {
	Kind   string `json:"kind"` // image, video, voice, audio, file, sticker, gif
	Mime   string `json:"mime,omitempty"`
	Name   string `json:"name,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Width  int    `json:"w,omitempty"`
	Height int    `json:"h,omitempty"`
	Secs   int    `json:"secs,omitempty"`
}

type Reply struct {
	ID         string `json:"id"`
	Sender     string `json:"sender,omitempty"`
	SenderName string `json:"sender_name,omitempty"`
	Text       string `json:"text,omitempty"`
}

type Reaction struct {
	Emoji string `json:"emoji"`
	By    string `json:"by"`
	Name  string `json:"name,omitempty"`
	TS    int64  `json:"ts,omitempty"`
}

type Msg struct {
	ID         string     `json:"id"` // "<author ACI>:<sent timestamp>"
	Chat       string     `json:"chat"`
	TS         int64      `json:"ts"` // ms
	Sender     string     `json:"sender"`
	SenderName string     `json:"sender_name,omitempty"`
	FromMe     bool       `json:"from_me,omitempty"`
	Kind       string     `json:"kind"` // text, media, sticker, system, deleted, call, contact, poll, unsupported
	Text       string     `json:"text,omitempty"`
	Media      []Media    `json:"media,omitempty"`
	Reply      *Reply     `json:"reply,omitempty"`
	Reactions  []Reaction `json:"reactions,omitempty"`
	Status     string     `json:"status,omitempty"` // pending, sent, delivered, read, failed
	Error      string     `json:"error,omitempty"`
	Edited     bool       `json:"edited,omitempty"`
	ViewOnce   bool       `json:"view_once,omitempty"`
	ExpiresIn  int        `json:"expires_in,omitempty"` // seconds (disappearing messages)
	ExpiresAt  int64      `json:"expires_at,omitempty"` // ms
	Mentions   bool       `json:"mentions_me,omitempty"`
}

func (m *Msg) withEmojiNames() *Msg {
	c := *m
	c.Text = emojiToNames(c.Text)
	if c.Reply != nil {
		r := *c.Reply
		r.Text = emojiToNames(r.Text)
		c.Reply = &r
	}
	if len(c.Reactions) > 0 {
		c.Reactions = append([]Reaction(nil), c.Reactions...)
		for i := range c.Reactions {
			c.Reactions[i].Emoji = emojiToNames(c.Reactions[i].Emoji)
		}
	}
	return &c
}

// Preview is the one-line summary used in the chat list and notifications.
func (m *Msg) Preview() string {
	switch m.Kind {
	case "deleted":
		return "This message was deleted"
	case "system", "call":
		return m.Text
	}
	label := ""
	if len(m.Media) > 0 {
		switch m.Media[0].Kind {
		case "image":
			label = "[Photo]"
		case "gif":
			label = "[GIF]"
		case "video":
			label = "[Video]"
		case "voice":
			label = "[Voice message]"
		case "audio":
			label = "[Audio]"
		case "sticker":
			label = "[Sticker]"
		default:
			label = "[File] " + m.Media[0].Name
		}
	}
	text := strings.Join(strings.Fields(m.Text), " ")
	switch {
	case label != "" && text != "":
		return label + " " + text
	case label != "":
		return label
	case text != "":
		return text
	case m.Kind == "contact":
		return "[Contact]"
	case m.Kind == "poll":
		return "[Poll]"
	}
	return "[Message]"
}

type Chat struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"` // dm, group, self
	Name        string `json:"name"`
	Phone       string `json:"phone,omitempty"`
	LastTS      int64  `json:"last_ts"`
	Preview     string `json:"preview,omitempty"`
	PreviewFrom string `json:"preview_from,omitempty"`
	PreviewMine bool   `json:"preview_mine,omitempty"`
	Unread      int    `json:"unread"`
	Pinned      bool   `json:"pinned,omitempty"`
	Muted       bool   `json:"muted,omitempty"`
	Archived    bool   `json:"archived,omitempty"`
	ExpireTimer int    `json:"expire_timer,omitempty"`
	Members     int    `json:"members,omitempty"`
	ReadOnly    bool   `json:"read_only,omitempty"`
	Request     bool   `json:"request,omitempty"`
}

// ---------------------------------------------------------------- storage

const schema = `
CREATE TABLE IF NOT EXISTS kp_chat (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	name TEXT NOT NULL DEFAULT '',
	last_ts INTEGER NOT NULL DEFAULT 0,
	preview TEXT NOT NULL DEFAULT '',
	preview_from TEXT NOT NULL DEFAULT '',
	preview_mine INTEGER NOT NULL DEFAULT 0,
	unread INTEGER NOT NULL DEFAULT 0,
	pinned INTEGER NOT NULL DEFAULT 0,
	muted INTEGER NOT NULL DEFAULT 0,
	archived INTEGER NOT NULL DEFAULT 0,
	expire_timer INTEGER NOT NULL DEFAULT 0,
	expire_version INTEGER NOT NULL DEFAULT 0,
	members INTEGER NOT NULL DEFAULT 0,
	read_only INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS kp_msg (
	chat TEXT NOT NULL,
	id TEXT NOT NULL,
	ts INTEGER NOT NULL,
	data TEXT NOT NULL,
	atts BLOB,
	expires_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (chat, id)
);
CREATE INDEX IF NOT EXISTS kp_msg_ts ON kp_msg (chat, ts);
CREATE INDEX IF NOT EXISTS kp_msg_id ON kp_msg (id);
CREATE INDEX IF NOT EXISTS kp_msg_exp ON kp_msg (expires_at) WHERE expires_at > 0;
CREATE TABLE IF NOT EXISTS kp_name (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	phone TEXT NOT NULL DEFAULT '',
	updated INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS kp_kv (
	k TEXT PRIMARY KEY,
	v TEXT NOT NULL
);
`

type Store struct {
	db *dbutil.Database
}

func (s *Store) Init(ctx context.Context) error {
	for _, stmt := range strings.Split(schema, ";\n") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := s.db.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetKV(ctx context.Context, k string) string {
	var v string
	_ = s.db.QueryRow(ctx, `SELECT v FROM kp_kv WHERE k=$1`, k).Scan(&v)
	return v
}

func (s *Store) SetKV(ctx context.Context, k, v string) {
	_, _ = s.db.Exec(ctx, `INSERT INTO kp_kv (k, v) VALUES ($1, $2) ON CONFLICT (k) DO UPDATE SET v=excluded.v`, k, v)
}

func (s *Store) Wipe(ctx context.Context) error {
	for _, t := range []string{"kp_chat", "kp_msg", "kp_name", "kp_kv"} {
		if _, err := s.db.Exec(ctx, "DELETE FROM "+t); err != nil {
			return err
		}
	}
	return nil
}

// ---- chats

const chatCols = `id, kind, name, last_ts, preview, preview_from, preview_mine, unread, pinned, muted, archived, expire_timer, members, read_only`

func scanChat(row dbutil.Scannable) (*Chat, error) {
	var c Chat
	err := row.Scan(&c.ID, &c.Kind, &c.Name, &c.LastTS, &c.Preview, &c.PreviewFrom, &c.PreviewMine, &c.Unread,
		&c.Pinned, &c.Muted, &c.Archived, &c.ExpireTimer, &c.Members, &c.ReadOnly)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) GetChat(ctx context.Context, id string) (*Chat, error) {
	c, err := scanChat(s.db.QueryRow(ctx, `SELECT `+chatCols+` FROM kp_chat WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (s *Store) ListChats(ctx context.Context, archived bool) ([]*Chat, error) {
	rows, err := s.db.Query(ctx, `SELECT `+chatCols+` FROM kp_chat WHERE archived=$1 AND (last_ts > 0 OR pinned=1)
		ORDER BY pinned DESC, last_ts DESC`, archived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Chat
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) EnsureChat(ctx context.Context, id, kind string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO kp_chat (id, kind) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`, id, kind)
	return err
}

func (s *Store) SetChatName(ctx context.Context, id, name string) (changed bool) {
	res, err := s.db.Exec(ctx, `UPDATE kp_chat SET name=$2 WHERE id=$1 AND name<>$2`, id, name)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

func (s *Store) SetChatGroupInfo(ctx context.Context, id, name string, members int, readOnly bool) {
	_, _ = s.db.Exec(ctx, `UPDATE kp_chat SET name=$2, members=$3, read_only=$4 WHERE id=$1`, id, name, members, readOnly)
}

func (s *Store) SetChatTimer(ctx context.Context, id string, timer, ver int) {
	_, _ = s.db.Exec(ctx, `UPDATE kp_chat SET expire_timer=$2, expire_version=$3 WHERE id=$1 AND expire_version<=$3`, id, timer, ver)
}

func (s *Store) ChatTimer(ctx context.Context, id string) (timer, ver int) {
	_ = s.db.QueryRow(ctx, `SELECT expire_timer, expire_version FROM kp_chat WHERE id=$1`, id).Scan(&timer, &ver)
	return
}

func (s *Store) SetChatFlag(ctx context.Context, id, col string, v bool) error {
	switch col {
	case "pinned", "muted", "archived":
	default:
		return errors.New("unknown flag")
	}
	_, err := s.db.Exec(ctx, `UPDATE kp_chat SET `+col+`=$2 WHERE id=$1`, id, v)
	return err
}

func (s *Store) BumpChat(ctx context.Context, m *Msg, unread bool) error {
	inc := 0
	if unread {
		inc = 1
	}
	_, err := s.db.Exec(ctx, `UPDATE kp_chat SET
			last_ts = CASE WHEN $2 >= last_ts THEN $2 ELSE last_ts END,
			preview = CASE WHEN $2 >= last_ts THEN $3 ELSE preview END,
			preview_from = CASE WHEN $2 >= last_ts THEN $4 ELSE preview_from END,
			preview_mine = CASE WHEN $2 >= last_ts THEN $5 ELSE preview_mine END,
			unread = unread + $6,
			archived = CASE WHEN $6 > 0 AND muted = 0 THEN 0 ELSE archived END
		WHERE id=$1`, m.Chat, m.TS, m.Preview(), m.Sender, m.FromMe && m.Kind != "system" && m.Kind != "call", inc)
	return err
}

// RefreshPreview recomputes the preview from the newest stored message.
func (s *Store) RefreshPreview(ctx context.Context, chat string) {
	msgs, _, err := s.ListMsgs(ctx, chat, 1, 0, "")
	if err != nil {
		return
	}
	if len(msgs) == 0 {
		_, _ = s.db.Exec(ctx, `UPDATE kp_chat SET preview='', preview_from='', preview_mine=0 WHERE id=$1`, chat)
		return
	}
	m := msgs[0]
	_, _ = s.db.Exec(ctx, `UPDATE kp_chat SET last_ts=$2, preview=$3, preview_from=$4, preview_mine=$5 WHERE id=$1`,
		chat, m.TS, m.Preview(), m.Sender, m.FromMe && m.Kind != "system" && m.Kind != "call")
}

func (s *Store) SetUnread(ctx context.Context, chat string, n int) {
	_, _ = s.db.Exec(ctx, `UPDATE kp_chat SET unread=$2 WHERE id=$1`, chat, n)
}

// ---- messages

func (s *Store) PutMsg(ctx context.Context, m *Msg, atts []byte) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO kp_msg (chat, id, ts, data, atts, expires_at) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (chat, id) DO UPDATE SET data=excluded.data, ts=excluded.ts, expires_at=excluded.expires_at,
			atts=COALESCE(excluded.atts, kp_msg.atts)`,
		m.Chat, m.ID, m.TS, string(data), atts, m.ExpiresAt)
	return err
}

func (s *Store) GetMsg(ctx context.Context, chat, id string) (*Msg, error) {
	var data string
	var err error
	if chat == "" {
		err = s.db.QueryRow(ctx, `SELECT data FROM kp_msg WHERE id=$1`, id).Scan(&data)
	} else {
		err = s.db.QueryRow(ctx, `SELECT data FROM kp_msg WHERE chat=$1 AND id=$2`, chat, id).Scan(&data)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var m Msg
	return &m, json.Unmarshal([]byte(data), &m)
}

func (s *Store) GetAtts(ctx context.Context, chat, id string) ([]byte, error) {
	var b []byte
	err := s.db.QueryRow(ctx, `SELECT atts FROM kp_msg WHERE chat=$1 AND id=$2`, chat, id).Scan(&b)
	return b, err
}

// ListMsgs returns up to limit messages before (beforeTS, beforeID), newest first.
func (s *Store) ListMsgs(ctx context.Context, chat string, limit int, beforeTS int64, beforeID string) ([]*Msg, bool, error) {
	if beforeTS <= 0 {
		beforeTS = 1 << 62
	}
	rows, err := s.db.Query(ctx, `SELECT data FROM kp_msg WHERE chat=$1 AND (ts < $2 OR (ts = $2 AND id < $3))
		ORDER BY ts DESC, id DESC LIMIT $4`, chat, beforeTS, beforeID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []*Msg
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, false, err
		}
		var m Msg
		if json.Unmarshal([]byte(data), &m) == nil {
			out = append(out, &m)
		}
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, rows.Err()
}

// UnreadIncoming lists incoming messages newer than the given time (ms).
func (s *Store) IncomingSince(ctx context.Context, chat string, sinceTS int64) ([]*Msg, error) {
	rows, err := s.db.Query(ctx, `SELECT data FROM kp_msg WHERE chat=$1 AND ts > $2 ORDER BY ts`, chat, sinceTS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Msg
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var m Msg
		if json.Unmarshal([]byte(data), &m) == nil && !m.FromMe && m.Kind != "system" {
			out = append(out, &m)
		}
	}
	return out, rows.Err()
}

func (s *Store) DeleteMsg(ctx context.Context, chat, id string) {
	_, _ = s.db.Exec(ctx, `DELETE FROM kp_msg WHERE chat=$1 AND id=$2`, chat, id)
}

func (s *Store) ClearChat(ctx context.Context, chat string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM kp_msg WHERE chat=$1`, chat); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `UPDATE kp_chat SET preview='', preview_from='', unread=0 WHERE id=$1`, chat)
	return err
}

// Expired removes disappearing messages whose time is up and returns them.
func (s *Store) Expired(ctx context.Context) (out []*Msg) {
	now := time.Now().UnixMilli()
	rows, err := s.db.Query(ctx, `SELECT data FROM kp_msg WHERE expires_at > 0 AND expires_at <= $1`, now)
	if err != nil {
		return nil
	}
	for rows.Next() {
		var data string
		if rows.Scan(&data) == nil {
			var m Msg
			if json.Unmarshal([]byte(data), &m) == nil {
				out = append(out, &m)
			}
		}
	}
	rows.Close()
	if len(out) > 0 {
		_, _ = s.db.Exec(ctx, `DELETE FROM kp_msg WHERE expires_at > 0 AND expires_at <= $1`, now)
	}
	return out
}

// TrimChat keeps only the newest keep messages of a chat.
func (s *Store) TrimChat(ctx context.Context, chat string, keep int) {
	_, _ = s.db.Exec(ctx, `DELETE FROM kp_msg WHERE chat=$1 AND id IN (
		SELECT id FROM kp_msg WHERE chat=$1 ORDER BY ts DESC LIMIT -1 OFFSET $2)`, chat, keep)
}

// ---- names

func (s *Store) PutName(ctx context.Context, id, name, phone string) (changed bool) {
	res, err := s.db.Exec(ctx, `INSERT INTO kp_name (id, name, phone, updated) VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE SET name=excluded.name, phone=excluded.phone, updated=excluded.updated
		WHERE kp_name.name<>excluded.name OR kp_name.phone<>excluded.phone`, id, name, phone, time.Now().Unix())
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

func (s *Store) LoadNames(ctx context.Context) map[string][2]string {
	out := map[string][2]string{}
	rows, err := s.db.Query(ctx, `SELECT id, name, phone FROM kp_name`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, name, phone string
		if rows.Scan(&id, &name, &phone) == nil {
			out[id] = [2]string{name, phone}
		}
	}
	return out
}

// ---------------------------------------------------------------- emoji

var maxEmojiRunes = func() int {
	n := 0
	for e := range emojiToName {
		if l := len([]rune(e)); l > n {
			n = l
		}
	}
	return n
}()

func emojiToNames(s string) string {
	if s == "" {
		return s
	}
	rs := []rune(s)
	var b strings.Builder
	for i := 0; i < len(rs); {
		r := rs[i]
		if r < 0x2000 && r != 0xa9 && r != 0xae && !(r >= '0' && r <= '9') && r != '#' && r != '*' {
			b.WriteRune(r)
			i++
			continue
		}
		matched := false
		for l := min(maxEmojiRunes, len(rs)-i); l >= 1; l-- {
			cand := string(rs[i : i+l])
			name, ok := emojiToName[cand]
			if !ok {
				name, ok = emojiToName[strings.ReplaceAll(cand, "️", "")]
			}
			if ok {
				b.WriteString(":" + name + ":")
				i += l
				matched = true
				break
			}
		}
		if !matched {
			if r != 0xfe0f && r != 0x200d {
				b.WriteRune(r)
			}
			i++
		}
	}
	return b.String()
}

var emojiNameRe = regexp.MustCompile(`:([a-z0-9_+\-&.'()!]+):`)

func namesToEmoji(s string) string {
	return emojiNameRe.ReplaceAllStringFunc(s, func(m string) string {
		if e, ok := nameToEmoji[strings.ToLower(m[1:len(m)-1])]; ok {
			return e
		}
		return m
	})
}
