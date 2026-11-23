// sig-helper keeps a Signal linked-device connection (signalmeow, from
// mautrix-signal) and serves a small JSON API on localhost; signal.koplugin
// draws the screens.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/web"
	_ "golang.org/x/crypto/x509roots/fallback" // Mozilla roots, for devices with an old CA store
)

// version is set at build time: -ldflags "-X main.version=1.2.3"
var version = "dev"

// ---------------------------------------------------------------- event bus

type Event struct {
	Seq    int64  `json:"seq"`
	Type   string `json:"type"` // msg, chat, chats, names, state, error
	Chat   string `json:"chat,omitempty"`
	Msg    *Msg   `json:"msg,omitempty"`
	New    bool   `json:"new,omitempty"`
	Reload bool   `json:"reload,omitempty"`
	Text   string `json:"text,omitempty"`
}

type Bus struct {
	mu     sync.Mutex
	seq    int64
	events []Event
	notify chan struct{}
}

const busKeep = 400

func NewBus() *Bus { return &Bus{notify: make(chan struct{})} }

func (b *Bus) Publish(e Event) {
	b.mu.Lock()
	b.seq++
	e.Seq = b.seq
	b.events = append(b.events, e)
	if len(b.events) > busKeep {
		b.events = append([]Event(nil), b.events[len(b.events)-busKeep:]...)
	}
	close(b.notify)
	b.notify = make(chan struct{})
	b.mu.Unlock()
}

// Since returns events after seq; reset is true when some were already dropped
// (or the helper restarted), so the client should reload everything.
func (b *Bus) Since(ctx context.Context, seq int64, wait time.Duration) (out []Event, last int64, reset bool) {
	deadline := time.After(wait)
	for {
		b.mu.Lock()
		last = b.seq
		if seq > last {
			b.mu.Unlock()
			return nil, last, true
		}
		if len(b.events) > 0 && b.events[0].Seq > seq+1 {
			b.mu.Unlock()
			return nil, last, true
		}
		for _, e := range b.events {
			if e.Seq > seq {
				out = append(out, e)
			}
		}
		ch := b.notify
		b.mu.Unlock()
		if len(out) > 0 || wait <= 0 {
			return
		}
		select {
		case <-ch:
		case <-deadline:
			return
		case <-ctx.Done():
			return
		}
		wait = 0
	}
}

// ---------------------------------------------------------------- HTTP helpers

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

type reqBody map[string]any

func (b reqBody) str(k string) string {
	if v, ok := b[k].(string); ok {
		return v
	}
	return ""
}

func (b reqBody) boolean(k string) bool {
	v, _ := b[k].(bool)
	return v
}

func (b reqBody) has(k string) bool {
	_, ok := b[k]
	return ok
}

func readBody(r *http.Request) (reqBody, error) {
	body := reqBody{}
	if r.Body == nil {
		return body, nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(&body); err != nil && !errors.Is(err, http.ErrBodyReadAfterClose) && err.Error() != "EOF" {
		return nil, fmt.Errorf("bad JSON: %w", err)
	}
	return body, nil
}

func qint(r *http.Request, k string, def int64) int64 {
	if v := r.URL.Query().Get(k); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// ---------------------------------------------------------------- server

type server struct {
	app   *App
	token string
	stop  chan struct{}
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
				fail(w, http.StatusUnauthorized, errors.New("unauthorized"))
				return
			}
		}
		next(w, r)
	}
}

func emojiNames(r *http.Request) bool { return r.Header.Get("X-Emoji") == "names" }

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	a := s.app
	h := func(path string, fn http.HandlerFunc) { mux.HandleFunc(path, s.auth(fn)) }
	post := func(path string, fn func(ctx context.Context, b reqBody, names bool) (any, error)) {
		h(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				fail(w, http.StatusMethodNotAllowed, errors.New("POST only"))
				return
			}
			b, err := readBody(r)
			if err != nil {
				fail(w, http.StatusBadRequest, err)
				return
			}
			res, err := fn(r.Context(), b, emojiNames(r))
			if err != nil {
				code := http.StatusBadRequest
				var he httpErr
				if errors.As(err, &he) {
					code = he.code
				}
				fail(w, code, err)
				return
			}
			if res == nil {
				res = map[string]bool{"ok": true}
			}
			writeJSON(w, http.StatusOK, res)
		})
	}

	h("/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.Status())
	})
	post("/login", func(ctx context.Context, b reqBody, _ bool) (any, error) {
		return nil, a.StartLogin(b.str("device_name"))
	})
	post("/login-cancel", func(ctx context.Context, b reqBody, _ bool) (any, error) {
		a.CancelLogin()
		return nil, nil
	})
	post("/logout", func(ctx context.Context, b reqBody, _ bool) (any, error) {
		return nil, a.Logout(ctx)
	})
	post("/reconnect", func(ctx context.Context, b reqBody, _ bool) (any, error) {
		a.Reconnect()
		return nil, nil
	})
	post("/shutdown", func(ctx context.Context, b reqBody, _ bool) (any, error) {
		go func() { time.Sleep(100 * time.Millisecond); close(s.stop) }()
		return nil, nil
	})
	h("/chats", func(w http.ResponseWriter, r *http.Request) {
		chats, err := a.Chats(r.Context(), r.URL.Query().Get("archived") == "1", emojiNames(r))
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"chats": chats})
	})
	h("/chat", func(w http.ResponseWriter, r *http.Request) {
		c, err := a.ChatInfo(r.Context(), r.URL.Query().Get("chat"), emojiNames(r))
		if err != nil {
			fail(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, c)
	})
	h("/messages", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		msgs, more, err := a.Messages(r.Context(), q.Get("chat"), int(qint(r, "limit", 40)), qint(r, "before_ts", 0), q.Get("before_id"), emojiNames(r))
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"messages": msgs, "more": more})
	})
	post("/send", func(ctx context.Context, b reqBody, names bool) (any, error) {
		return a.Send(b.str("chat"), b.str("text"), b.str("reply_to"), b.str("retry_id"), names)
	})
	post("/edit", func(ctx context.Context, b reqBody, names bool) (any, error) {
		return nil, a.Edit(b.str("chat"), b.str("id"), b.str("text"))
	})
	post("/delete", func(ctx context.Context, b reqBody, _ bool) (any, error) {
		return nil, a.Delete(b.str("chat"), b.str("id"), b.boolean("for_everyone"))
	})
	post("/react", func(ctx context.Context, b reqBody, _ bool) (any, error) {
		return nil, a.React(b.str("chat"), b.str("id"), b.str("emoji"))
	})
	post("/read", func(ctx context.Context, b reqBody, _ bool) (any, error) {
		return nil, a.MarkRead(b.str("chat"))
	})
	post("/chat-flag", func(ctx context.Context, b reqBody, _ bool) (any, error) {
		return nil, a.SetChatFlags(ctx, b)
	})
	post("/clear-chat", func(ctx context.Context, b reqBody, _ bool) (any, error) {
		return nil, a.ClearChat(ctx, b.str("chat"))
	})
	post("/lookup", func(ctx context.Context, b reqBody, _ bool) (any, error) {
		return a.Lookup(ctx, b.str("phone"), b.str("username"))
	})
	h("/contacts", func(w http.ResponseWriter, r *http.Request) {
		cs, err := a.Contacts(r.Context())
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"contacts": cs})
	})
	post("/open", func(ctx context.Context, b reqBody, names bool) (any, error) {
		return a.OpenChat(ctx, b.str("chat"), names)
	})
	h("/media", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		data, mime, err := a.Media(r.Context(), q.Get("chat"), q.Get("id"), int(qint(r, "idx", 0)), int(qint(r, "max", 0)), q.Get("cached") == "1")
		if errors.Is(err, errNotReady) {
			fail(w, http.StatusAccepted, err)
			return
		} else if err != nil {
			fail(w, http.StatusNotFound, err)
			return
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		_, _ = w.Write(data)
	})
	h("/events", func(w http.ResponseWriter, r *http.Request) {
		since := qint(r, "since", 0)
		wait := time.Duration(qint(r, "wait", 0)) * time.Second
		if wait > 60*time.Second {
			wait = 60 * time.Second
		}
		evs, last, reset := a.bus.Since(r.Context(), since, wait)
		if emojiNames(r) {
			for i := range evs {
				if evs[i].Msg != nil {
					evs[i].Msg = evs[i].Msg.withEmojiNames()
				}
			}
		}
		if evs == nil {
			evs = []Event{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"events": evs, "seq": last, "reset": reset})
	})
	return mux
}

type httpErr struct {
	code int
	msg  string
}

func (e httpErr) Error() string { return e.msg }

// ---------------------------------------------------------------- main

func main() {
	dataDir := flag.String("data", "", "data folder")
	listen := flag.String("listen", "127.0.0.1:18782", "API address")
	token := flag.String("token", "", "bearer token required from clients")
	parentPID := flag.Int("parent-pid", 0, "exit when this process ends")
	memLimit := flag.Int("mem-limit", 96, "soft memory limit in MB")
	logLevel := flag.String("log", "INFO", "DEBUG, INFO, WARN or ERROR")
	deviceName := flag.String("device-name", "Kindle (KOReader)", "name shown in Signal's linked devices")
	demo := flag.Bool("demo", false, "fake account for UI testing (demo builds only)")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println("sig-helper", version)
		return
	}
	if *dataDir == "" {
		fmt.Fprintln(os.Stderr, "-data is required")
		os.Exit(2)
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad -listen:", err)
		os.Exit(2)
	}
	if ip := net.ParseIP(host); (ip == nil || !ip.IsLoopback()) && host != "localhost" && *token == "" {
		fmt.Fprintln(os.Stderr, "a non-localhost -listen address needs -token")
		os.Exit(2)
	}
	if os.Getenv("HTTPS_PROXY") != "" || os.Getenv("https_proxy") != "" {
		// Signal's own transport ignores proxy settings; honour them if set.
		if t, ok := web.SignalHTTPClient.Transport.(*http.Transport); ok {
			t.Proxy = http.ProxyFromEnvironment
		}
	}
	if *memLimit > 0 {
		debug.SetMemoryLimit(int64(*memLimit) << 20)
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	lvl, err := zerolog.ParseLevel(strings.ToLower(*logLevel))
	if err != nil {
		lvl = zerolog.InfoLevel
	}
	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, NoColor: true, TimeFormat: "15:04:05"}).
		Level(lvl).With().Timestamp().Logger()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Error().Err(err).Msg("Can't listen")
		os.Exit(3)
	}

	app, err := NewApp(*dataDir, *deviceName, log)
	if err != nil {
		log.Error().Err(err).Msg("Can't open data")
		os.Exit(1)
	}
	if *demo {
		if !startDemo(app) {
			log.Error().Msg("This build has no demo mode (build with -tags demo)")
			os.Exit(2)
		}
	} else {
		app.Start()
	}

	s := &server{app: app, token: *token, stop: make(chan struct{})}
	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	log.Info().Str("version", version).Str("listen", ln.Addr().String()).Msg("sig-helper ready")

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	parentGone := make(chan struct{})
	if *parentPID > 0 {
		go func() {
			for {
				time.Sleep(3 * time.Second)
				if err := syscall.Kill(*parentPID, 0); err != nil && errors.Is(err, syscall.ESRCH) {
					close(parentGone)
					return
				}
			}
		}()
	}
	select {
	case <-sigs:
	case <-s.stop:
	case <-parentGone:
		log.Info().Msg("KOReader exited")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = srv.Shutdown(ctx)
	cancel()
	app.Close()
	log.Info().Msg("Bye")
}
