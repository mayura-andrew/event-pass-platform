package httphandler

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nasa-spaceapps/nsac-qr/internal/domain"
)

// manifestTTL is how stale the cached directory may be. Delegates are added
// by the seed script before the event, not by scans, so 30 s is invisible.
const manifestTTL = 30 * time.Second

// manifestCache holds the encoded directory. Every scanner refreshes it when
// it comes back online or to the foreground, so a crowd regaining Wi-Fi asks
// for it at once. Encoding it per request (query + 1 MB of JSON + gzip) cost
// about 60 ms of CPU each, which is 3.5 s of CPU per 60 phones on a one-vCPU
// container, stalling scan uploads meanwhile.
type manifestCache struct {
	mu  sync.Mutex     // also makes concurrent misses wait for one rebuild
	cur *manifestEntry // replaced on rebuild, never modified
}

type manifestEntry struct {
	built time.Time
	body  []byte // JSON
	gz    []byte // gzip of body
	etag  string
	count int
}

func (c *manifestCache) get(ctx context.Context, load func(context.Context) ([]domain.ParticipantManifest, error)) (*manifestEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur != nil && time.Since(c.cur.built) < manifestTTL {
		return c.cur, nil
	}
	data, err := load(ctx)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression) // paid once per TTL
	_, _ = gz.Write(body)
	if err := gz.Close(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	c.cur = &manifestEntry{
		built: time.Now(), body: body, gz: buf.Bytes(), count: len(data),
		etag: `"` + hex.EncodeToString(sum[:16]) + `"`,
	}
	return c.cur, nil
}

// GET /api/v1/participants/manifest
//
// Returns the full confirmed directory. Supports If-None-Match so scanners
// that already hold the current manifest get a 304, and gzips the body because
// a few thousand delegates is ~1-2 MB of JSON over venue Wi-Fi.
func (h *Handler) manifest(w http.ResponseWriter, r *http.Request) {
	m, err := h.manifests.get(r.Context(), h.Sync.GetConfirmedManifest)
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	body, gzBody, etag, count := m.body, m.gz, m.etag, m.count

	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Accept-Encoding, Authorization")
	w.Header().Set("X-Manifest-Count", strconv.Itoa(count))
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(gzBody)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gzBody)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// GET /api/v1/participants/pass?id={membership_id}&email={email}
//
// Public endpoint. Both fields are required, and a wrong email returns the
// same 404 as an unknown ID so the endpoint can't be used to enumerate.
func (h *Handler) pass(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := h.Participants.GetPass(r.Context(), q.Get("id"), q.Get("email"))
	if err != nil {
		writeDomainError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, p)
}

// GET /api/v1/config/public: browser-safe settings for the portal.
func (h *Handler) publicConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"google_maps_api_key": h.MapsAPIKey,
		"google_maps_map_id":  h.MapsMapID,
	})
}
