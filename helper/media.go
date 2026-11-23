package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
	"google.golang.org/protobuf/proto"

	"go.mau.fi/mautrix-signal/pkg/signalmeow"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/protobuf/signalpb"
)

var mediaLock sync.Mutex // one download/decode at a time keeps memory use low

var errNotReady = errors.New("downloading; try again soon")

// background downloads started by cachedOnly requests
var (
	bgMu   sync.Mutex
	bgBusy = map[string]bool{}
)

// Photos bigger than this aren't fetched automatically for inline previews.
const autoDownloadMax = 8 << 20

const maxMediaBytes = 100 << 20

// Media returns attachment idx of a message. With maxDim > 0, images are
// scaled to fit maxDim×maxDim and returned as grayscale JPEG (e-ink friendly).
func (a *App) Media(ctx context.Context, chat, id string, idx, maxDim int, cachedOnly bool) ([]byte, string, error) {
	m := a.findMsg(ctx, chat, id)
	if m == nil || idx < 0 || idx >= len(m.Media) {
		return nil, "", errors.New("no such attachment")
	}
	md := m.Media[idx]
	mime := md.Mime
	if mime == "" {
		mime = "application/octet-stream"
	}
	isImage := md.Kind == "image" || md.Kind == "sticker" || md.Kind == "gif"
	if maxDim > 0 && !isImage {
		maxDim = 0
	}
	dir := filepath.Join(a.dataDir, "media")
	_ = os.MkdirAll(dir, 0o700)
	key := sha256.Sum256([]byte(m.Chat + "|" + m.ID + "|" + strconv.Itoa(idx)))
	orig := filepath.Join(dir, hex.EncodeToString(key[:12]))
	scaled := orig + "-" + strconv.Itoa(maxDim) + ".jpg"

	if cachedOnly {
		if _, err := os.Stat(scaled); err != nil {
			if _, err := os.Stat(orig); err != nil {
				if md.Size > autoDownloadMax {
					return nil, "", errors.New("too large for a preview")
				}
				a.backgroundFetch(m, idx, maxDim)
				return nil, "", errNotReady
			}
		}
	}
	mediaLock.Lock()
	defer mediaLock.Unlock()
	if maxDim > 0 {
		if b, err := os.ReadFile(scaled); err == nil {
			return b, "image/jpeg", nil
		}
	}
	if _, err := os.Stat(orig); err != nil {
		if a.demo {
			if err := demoMedia(m, idx, orig); err != nil {
				return nil, "", err
			}
		} else if err := a.download(ctx, m, idx, orig); err != nil {
			return nil, "", err
		}
	}
	if maxDim > 0 {
		b, err := scaleImage(orig, maxDim)
		if err == nil {
			_ = os.WriteFile(scaled, b, 0o600)
			return b, "image/jpeg", nil
		}
		a.log.Warn().Err(err).Msg("Image scaling failed; sending original")
	}
	b, err := os.ReadFile(orig)
	return b, mime, err
}

// backgroundFetch downloads (and scales) an attachment, then tells the plugin.
func (a *App) backgroundFetch(m *Msg, idx, maxDim int) {
	key := m.Chat + "|" + m.ID + "|" + strconv.Itoa(idx)
	bgMu.Lock()
	if bgBusy[key] {
		bgMu.Unlock()
		return
	}
	bgBusy[key] = true
	bgMu.Unlock()
	go func() {
		defer func() {
			bgMu.Lock()
			delete(bgBusy, key)
			bgMu.Unlock()
		}()
		if _, _, err := a.Media(a.ctx, m.Chat, m.ID, idx, maxDim, false); err != nil {
			a.log.Debug().Err(err).Str("msg", m.ID).Msg("Preview download failed")
			return
		}
		cur := a.findMsg(a.ctx, m.Chat, m.ID)
		if cur != nil {
			a.names.fill(cur)
			a.bus.Publish(Event{Type: "media", Chat: m.Chat, Msg: cur})
		}
	}()
}

func (a *App) download(ctx context.Context, m *Msg, idx int, dest string) error {
	raw, err := a.st.GetAtts(ctx, m.Chat, m.ID)
	if err != nil || len(raw) == 0 {
		return errors.New("attachment details missing")
	}
	var atts []storedAtt
	if err := json.Unmarshal(raw, &atts); err != nil || idx >= len(atts) {
		return errors.New("attachment details missing")
	}
	var ptr signalpb.AttachmentPointer
	if err := proto.Unmarshal(atts[idx].P, &ptr); err != nil {
		return err
	}
	if ptr.GetSize() > maxMediaBytes {
		return fmt.Errorf("file too large (%d MB)", ptr.GetSize()>>20)
	}
	if a.client() == nil {
		return errors.New("not linked")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	tmp, err := os.CreateTemp(filepath.Dir(dest), "dl-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = signalmeow.DownloadAttachmentWithPointer(ctx, &ptr, atts[idx].H, tmp)
	if err != nil && strings.Contains(err.Error(), "allocate") && ptr.GetSize() <= 32<<20 {
		// Some filesystems (FAT on old kernels) refuse pre-allocation: download into memory instead.
		a.log.Warn().Err(err).Msg("Download to file failed; retrying in memory")
		var data []byte
		if data, err = signalmeow.DownloadAttachmentWithPointer(ctx, &ptr, atts[idx].H, nil); err == nil {
			if _, err = tmp.Seek(0, io.SeekStart); err == nil {
				if err = tmp.Truncate(0); err == nil {
					_, err = tmp.Write(data)
				}
			}
		}
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if errors.Is(err, signalmeow.ErrAttachmentNotFound) {
			return errors.New("this attachment has expired on Signal's servers")
		}
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

func scaleImage(path string, maxDim int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, err
	}
	if cfg.Width*cfg.Height > 40_000_000 {
		return nil, errors.New("image too large")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	src, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > maxDim || h > maxDim {
		if w >= h {
			h, w = max(1, h*maxDim/w), maxDim
		} else {
			w, h = max(1, w*maxDim/h), maxDim
		}
	}
	dst := image.NewGray(image.Rect(0, 0, w, h))
	// Transparent stickers: paint white first
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
