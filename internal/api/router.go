package api

import (
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gicravets/my-sensein-book-backend/internal/epub"
	"github.com/gicravets/my-sensein-book-backend/internal/fb2"
	"github.com/gicravets/my-sensein-book-backend/internal/model"
	"github.com/gicravets/my-sensein-book-backend/internal/store"
)

// Config holds the server's runtime configuration.
type Config struct {
	BookFile    []byte // fallback EPUB served when a book has no stored file
	RequireAuth bool
	MasterKey   string
	Demo        bool   // read-only demo deployment (writes are blocked)
	Version     string // build version, surfaced by GET /version
	Repo        string // GitHub owner/repo for the update check
	MetaBase    string // metadata provider base URL (Open Library); overridable for tests
	WatchDir    string // watched folder auto-imported into the library
}

// Server holds dependencies for the HTTP handlers.
type Server struct {
	st          *store.Store
	bookFile    []byte
	requireAuth bool
	masterKey   string
	demo        bool
	version     string
	repo        string
	metaBase    string
	watchDir    string
	fbMu        sync.Mutex
	fbCache     map[string][]byte // id -> FB2-converted EPUB
	upMu        sync.Mutex
	upCache     *updateInfo // cached GitHub update check
	upAt        time.Time
}

// NewRouter wires routes. Shape follows the frontend contract (Komga-style REST
// + CWA data model). When requireAuth is true, /api/v1/* needs a valid X-API-Key
// (device key, DB admin key, or masterKey); /health, /setup, /version, /update and
// device registration stay open. In demo mode all writes are blocked.
func NewRouter(st *store.Store, cfg Config) http.Handler {
	metaBase := cfg.MetaBase
	if metaBase == "" {
		metaBase = "https://openlibrary.org"
	}
	s := &Server{
		st: st, bookFile: cfg.BookFile, requireAuth: cfg.RequireAuth,
		masterKey: cfg.MasterKey, demo: cfg.Demo, version: cfg.Version, repo: cfg.Repo,
		metaBase: metaBase, watchDir: cfg.WatchDir,
	}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /api/v1/setup", s.setupStatus)
	mux.HandleFunc("POST /api/v1/setup/claim", s.setupClaim)
	mux.HandleFunc("GET /api/v1/version", s.serverVersion)
	mux.HandleFunc("GET /api/v1/update", s.updateCheck)
	mux.HandleFunc("POST /api/v1/auth/device", s.registerDevice)
	mux.HandleFunc("POST /api/v1/auth/pair", s.createPairing)
	mux.HandleFunc("POST /api/v1/auth/pair/claim", s.claimPairing)
	mux.HandleFunc("GET /api/v1/auth/pair/status", s.pairingStatus)

	mux.HandleFunc("GET /api/v1/books", s.listBooks)
	mux.HandleFunc("GET /api/v1/search", s.search)
	mux.HandleFunc("GET /api/v1/series", s.listSeries)
	mux.HandleFunc("POST /api/v1/books", s.createBook)
	mux.HandleFunc("POST /api/v1/library/scan", s.scanLibrary)
	mux.HandleFunc("GET /api/v1/books/{id}", s.getBook)
	mux.HandleFunc("DELETE /api/v1/books/{id}", s.deleteBook)
	mux.HandleFunc("GET /api/v1/sync", s.syncDelta)
	mux.HandleFunc("POST /api/v1/books/{id}/enrich", s.enrichBook)
	mux.HandleFunc("GET /api/v1/books/{id}/file", s.getBookFile)
	mux.HandleFunc("GET /api/v1/books/{id}/cover", s.getBookCover)
	mux.HandleFunc("GET /api/v1/devices", s.listDevices)
	mux.HandleFunc("DELETE /api/v1/devices/{id}", s.deleteDevice)
	mux.HandleFunc("GET /api/v1/users", s.listUsers)
	mux.HandleFunc("POST /api/v1/users", s.createUser)

	mux.HandleFunc("GET /api/v1/shelves", s.listShelves)
	mux.HandleFunc("POST /api/v1/shelves", s.createShelf)
	mux.HandleFunc("PATCH /api/v1/shelves/{id}", s.patchShelf)
	mux.HandleFunc("DELETE /api/v1/shelves/{id}", s.deleteShelf)
	mux.HandleFunc("POST /api/v1/shelves/{id}/books/{bookId}", s.addBookToShelf)
	mux.HandleFunc("DELETE /api/v1/shelves/{id}/books/{bookId}", s.removeBookFromShelf)

	mux.HandleFunc("GET /api/v1/smart-shelves", s.listSmartShelves)
	mux.HandleFunc("POST /api/v1/smart-shelves", s.createSmartShelf)
	mux.HandleFunc("DELETE /api/v1/smart-shelves/{id}", s.deleteSmartShelf)
	mux.HandleFunc("GET /api/v1/smart-shelves/{id}/books", s.smartShelfBooks)

	mux.HandleFunc("GET /api/v1/books/{id}/progression", s.getProgression)
	mux.HandleFunc("PUT /api/v1/books/{id}/progression", s.putProgression)
	mux.HandleFunc("PATCH /api/v1/books/{id}/read-progress", s.patchReadProgress)
	mux.HandleFunc("PATCH /api/v1/books/{id}/rating", s.patchRating)
	mux.HandleFunc("PATCH /api/v1/books/{id}/archived", s.patchArchived)

	mux.HandleFunc("GET /api/v1/highlights", s.listHighlights)
	mux.HandleFunc("POST /api/v1/highlights", s.createHighlight)
	mux.HandleFunc("PATCH /api/v1/highlights/{id}", s.patchHighlight)
	mux.HandleFunc("DELETE /api/v1/highlights/{id}", s.deleteHighlight)

	mux.HandleFunc("GET /api/v1/bookmarks", s.listBookmarks)
	mux.HandleFunc("POST /api/v1/bookmarks", s.createBookmark)
	mux.HandleFunc("DELETE /api/v1/bookmarks/{id}", s.deleteBookmark)

	mux.HandleFunc("GET /api/v1/preferences", s.getPreferences)
	mux.HandleFunc("PUT /api/v1/preferences", s.putPreferences)

	return cors(logging(s.auth(s.demoGuard(mux))))
}

// openPath reports whether a path bypasses the auth gate (bootstrap + auth flows).
func openPath(p string) bool {
	return strings.HasPrefix(p, "/api/v1/auth/") ||
		strings.HasPrefix(p, "/api/v1/setup") ||
		p == "/api/v1/version" || p == "/api/v1/update"
}

// auth gate: when enabled, /api/v1/* (except open bootstrap paths) needs a valid key.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAuth || r.Method == http.MethodOptions ||
			!strings.HasPrefix(r.URL.Path, "/api/v1/") || openPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		key := r.Header.Get("X-API-Key")
		if s.validAdmin(key) || s.st.ValidKey(key) {
			next.ServeHTTP(w, r)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or missing X-API-Key"})
	})
}

// validAdmin matches the env master key or the DB admin key set during setup.
func (s *Server) validAdmin(key string) bool {
	if key == "" {
		return false
	}
	if s.masterKey != "" && key == s.masterKey {
		return true
	}
	if ak, ok, _ := s.st.GetSetting("admin_key"); ok && key == ak {
		return true
	}
	return false
}

// demoGuard blocks mutating requests in demo mode (read-only deployment).
func (s *Server) demoGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.demo && r.Method != http.MethodGet && r.Method != http.MethodOptions &&
			strings.HasPrefix(r.URL.Path, "/api/v1/") && !strings.HasPrefix(r.URL.Path, "/api/v1/auth/") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "demo mode: read-only"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// GET /api/v1/search?q=&limit= — full-text over book metadata + saved highlights (FTS5).
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	res, err := s.st.Search(r.URL.Query().Get("q"), limit)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// POST /api/v1/books/{id}/enrich — fetch a cover + description from Open Library by
// title/author and fill any missing fields. Best-effort: returns {book, enriched}.
func (s *Server) enrichBook(w http.ResponseWriter, r *http.Request) {
	b, ok, err := s.st.GetBook(r.PathValue("id"))
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	author := ""
	if len(b.Authors) > 0 {
		author = b.Authors[0]
	}
	meta, found := searchOpenLibrary(&http.Client{Timeout: 8 * time.Second}, s.metaBase, b.Title, author)
	enriched := false
	if found {
		if meta.Description != "" && (b.Description == nil || *b.Description == "") {
			b.Description = &meta.Description
			enriched = true
		}
		if meta.CoverURL != "" && (b.CoverURL == nil || *b.CoverURL == "") {
			b.CoverURL = &meta.CoverURL
			enriched = true
		}
		if enriched {
			_ = s.st.SaveBook(b)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"book": b, "enriched": enriched})
}

type metaResult struct {
	Description string
	CoverURL    string
}

// searchOpenLibrary queries Open Library's search.json for a cover id + first sentence.
func searchOpenLibrary(client *http.Client, base, title, author string) (metaResult, bool) {
	if title == "" {
		return metaResult{}, false
	}
	u := base + "/search.json?limit=1&title=" + url.QueryEscape(title)
	if author != "" {
		u += "&author=" + url.QueryEscape(author)
	}
	resp, err := client.Get(u)
	if err != nil {
		return metaResult{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return metaResult{}, false
	}
	var data struct {
		Docs []struct {
			CoverI        int      `json:"cover_i"`
			FirstSentence []string `json:"first_sentence"`
		} `json:"docs"`
	}
	if json.NewDecoder(resp.Body).Decode(&data) != nil || len(data.Docs) == 0 {
		return metaResult{}, false
	}
	d := data.Docs[0]
	var res metaResult
	if len(d.FirstSentence) > 0 {
		res.Description = d.FirstSentence[0]
	}
	if d.CoverI > 0 {
		res.CoverURL = fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-L.jpg", d.CoverI)
	}
	return res, res.Description != "" || res.CoverURL != ""
}

// GET /api/v1/series — multi-volume groupings (book metadata Series). Books via ?series=.
func (s *Server) listSeries(w http.ResponseWriter, r *http.Request) {
	series, err := s.st.ListSeries()
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": series, "totalElements": len(series)})
}

// DELETE /api/v1/books/{id} — soft delete; records a tombstone so the removal syncs.
func (s *Server) deleteBook(w http.ResponseWriter, r *http.Request) {
	ok, err := s.st.SoftDeleteBook(r.PathValue("id"))
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/v1/sync?since= — library delta since a device's sync point (ref: Komga SYNC_POINT).
// Returns changed/added books, ids removed since, and the serverTime to store as the next point.
func (s *Server) syncDelta(w http.ResponseWriter, r *http.Request) {
	changed, removed, serverTime, err := s.st.SyncDelta(r.URL.Query().Get("since"))
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"serverTime": serverTime, "books": changed, "removed": removed,
	})
}

// ---------- smart shelves (dynamic, rule-based; ref: CWA magic_shelf) ----------

func (s *Server) listSmartShelves(w http.ResponseWriter, r *http.Request) {
	items, err := s.st.ListSmartShelves()
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": items, "totalElements": len(items)})
}

func (s *Server) createSmartShelf(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string          `json:"name"`
		Rules store.BookQuery `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		badRequest(w, fmt.Errorf("name required"))
		return
	}
	sh, err := s.st.CreateSmartShelf(strings.TrimSpace(body.Name), body.Rules)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sh)
}

func (s *Server) deleteSmartShelf(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteSmartShelf(r.PathValue("id")); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/v1/smart-shelves/{id}/books — evaluate the rule into a page of books.
func (s *Server) smartShelfBooks(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("size"))
	res, ok, err := s.st.SmartShelfBooks(s.currentUser(r), r.PathValue("id"), page, size)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- reader preferences (per-user sync, Wave 1; ref: CWA CLIENT_SETTINGS_USER) ----------

// currentUser resolves the request's API key to a user id (Wave 0: always the owner).
func (s *Server) currentUser(r *http.Request) string {
	return s.st.UserForKey(r.Header.Get("X-API-Key"))
}

// GET /api/v1/preferences — the user's reader settings (theme/font/mode/…) as a JSON object.
func (s *Server) getPreferences(w http.ResponseWriter, r *http.Request) {
	prefs, err := s.st.GetPreferences(s.currentUser(r))
	if err != nil {
		serverError(w, err)
		return
	}
	if prefs == nil {
		prefs = map[string]json.RawMessage{}
	}
	writeJSON(w, http.StatusOK, prefs)
}

// PUT /api/v1/preferences — upsert reader settings (per-key last-writer-wins); returns the merged set.
func (s *Server) putPreferences(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	user := s.currentUser(r)
	if err := s.st.PutPreferences(user, body); err != nil {
		serverError(w, err)
		return
	}
	prefs, err := s.st.GetPreferences(user)
	if err != nil {
		serverError(w, err)
		return
	}
	if prefs == nil {
		prefs = map[string]json.RawMessage{}
	}
	writeJSON(w, http.StatusOK, prefs)
}

// ---------- setup wizard / version / update (ref: Komga claim + announcements, CWA update channel) ----------

// isClaimed reports whether initial setup is done (an admin exists, demo, or env master key).
func (s *Server) isClaimed() bool {
	if s.demo || s.masterKey != "" {
		return true
	}
	if _, ok, _ := s.st.GetSetting("admin_key"); ok {
		return true
	}
	return s.st.HasAnyDevice()
}

// GET /api/v1/setup — first-run status (open). Clients show a wizard when claimed=false.
func (s *Server) setupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"claimed":      s.isClaimed(),
		"demo":         s.demo,
		"requiresAuth": s.requireAuth,
		"version":      s.version,
	})
}

// POST /api/v1/setup/claim — create the admin key on a fresh server (ref: Komga claim).
// Body {apiKey?}; if omitted a key is generated. Returns the admin apiKey once.
func (s *Server) setupClaim(w http.ResponseWriter, r *http.Request) {
	if s.isClaimed() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already set up"})
		return
	}
	var body struct {
		APIKey string `json:"apiKey"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	key := strings.TrimSpace(body.APIKey)
	if key == "" {
		b := make([]byte, 24)
		_, _ = cryptorand.Read(b)
		key = hex.EncodeToString(b)
	}
	if err := s.st.SetSetting("admin_key", key); err != nil {
		serverError(w, err)
		return
	}
	_ = s.st.SetSetting("claimed", "true")
	writeJSON(w, http.StatusCreated, map[string]any{"apiKey": key, "claimed": true})
}

// GET /api/v1/version — build version (open).
func (s *Server) serverVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"version": s.version, "demo": s.demo})
}

type updateInfo struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"updateAvailable"`
	URL             string `json:"url"`
}

// GET /api/v1/update — latest GitHub release vs current (cached ~1h). Notify-only,
// like Komga announcements (the actual update is a Docker image pull).
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	s.upMu.Lock()
	if s.upCache != nil && time.Since(s.upAt) < time.Hour {
		info := *s.upCache
		s.upMu.Unlock()
		writeJSON(w, http.StatusOK, info)
		return
	}
	s.upMu.Unlock()

	info := updateInfo{Current: s.version}
	if s.repo != "" {
		if latest, url, ok := fetchLatestRelease(s.repo); ok {
			info.Latest, info.URL = latest, url
			info.UpdateAvailable = latest != "" && latest != s.version && s.version != "dev"
		}
	}
	s.upMu.Lock()
	s.upCache, s.upAt = &info, time.Now()
	s.upMu.Unlock()
	writeJSON(w, http.StatusOK, info)
}

func fetchLatestRelease(repo string) (tag, url string, ok bool) {
	c := &http.Client{Timeout: 6 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/"+repo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.Do(req)
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", false
	}
	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if json.NewDecoder(resp.Body).Decode(&rel) != nil {
		return "", "", false
	}
	return rel.TagName, rel.HTMLURL, true
}

// GET /api/v1/users — family users; POST creates one (admin picks who a device pairs to).
func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.st.ListUsers()
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": users, "totalElements": len(users)})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		badRequest(w, fmt.Errorf("name required"))
		return
	}
	u, err := s.st.CreateUser(strings.TrimSpace(body.Name))
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) registerDevice(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		UserID string `json:"userId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if strings.TrimSpace(body.Name) == "" {
		body.Name = "device"
	}
	// when auth is enforced, registering a new device requires the master key
	if s.requireAuth && s.masterKey != "" && r.Header.Get("X-API-Key") != s.masterKey {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "master key required to register"})
		return
	}
	d, err := s.st.RegisterDevice(strings.TrimSpace(body.Name), body.UserID)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

// POST /api/v1/auth/pair?userId= — web (authed) creates a short-lived pairing token + QR payload.
func (s *Server) createPairing(w http.ResponseWriter, r *http.Request) {
	if s.requireAuth && s.masterKey != "" && r.Header.Get("X-API-Key") != s.masterKey {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "master key required"})
		return
	}
	p, err := s.st.CreatePairing(5*time.Minute, r.URL.Query().Get("userId"))
	if err != nil {
		serverError(w, err)
		return
	}
	serverURL := fmt.Sprintf("http://%s", r.Host)
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":   p.Token,
		"expires": p.Expires,
		"qr":      map[string]string{"url": serverURL, "t": p.Token}, // encode this JSON in the QR
	})
}

// POST /api/v1/auth/pair/claim — iOS scans QR, exchanges token for a device key.
func (s *Server) claimPairing(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
		Name  string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		badRequest(w, fmt.Errorf("token required"))
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		body.Name = "iOS device"
	}
	d, ok, err := s.st.ClaimPairing(body.Token, strings.TrimSpace(body.Name))
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid, expired or already-used token"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deviceId": d.ID, "deviceName": d.Name, "key": d.Key})
}

// GET /api/v1/auth/pair/status?token= — web polls to learn when the device linked.
func (s *Server) pairingStatus(w http.ResponseWriter, r *http.Request) {
	status, name := s.st.PairingStatus(r.URL.Query().Get("token"))
	writeJSON(w, http.StatusOK, map[string]string{"status": status, "deviceName": name})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "my-sensein-book-backend"})
}

func (s *Server) listBooks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("size"))
	res, err := s.st.ListBooks(s.currentUser(r), store.BookQuery{
		Search: q.Get("search"), Shelf: q.Get("shelf"), Tag: q.Get("tag"),
		Author: q.Get("author"), Series: q.Get("series"),
		Language: q.Get("language"), Publisher: q.Get("publisher"), Format: q.Get("format"),
		Filter: q.Get("filter"), Sort: q.Get("sort"), Page: page, Size: size,
	})
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) getBook(w http.ResponseWriter, r *http.Request) {
	b, ok, err := s.st.GetBook(r.PathValue("id"))
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	b.ReadProgress = s.st.GetProgression(s.currentUser(r), b.ID) // per-user overlay
	writeJSON(w, http.StatusOK, b)
}

// POST /api/v1/books — multipart upload of an EPUB/FB2; parses metadata + cover.
func (s *Server) createBook(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(128 << 20); err != nil {
		badRequest(w, err)
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		badRequest(w, err)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		serverError(w, err)
		return
	}
	b, created, err := importBytes(s.st, hdr.Filename, data, r.Host)
	if err != nil {
		serverError(w, err)
		return
	}
	if created {
		writeJSON(w, http.StatusCreated, b)
	} else {
		writeJSON(w, http.StatusOK, b) // dedup: identical file already present
	}
}

// importBytes ingests one book file: hash + dedup, parse metadata + cover, persist.
// Shared by the upload endpoint and the watched-folder scanner. created=false on dedup.
func importBytes(st *store.Store, filename string, data []byte, host string) (model.Book, bool, error) {
	hash := fmt.Sprintf("%x", sha256.Sum256(data))
	if existing, ok := st.FindBookByHash(hash); ok {
		return existing, false, nil
	}
	meta, _ := epub.Parse(data) // best-effort (EPUB)
	format := model.FormatEPUB
	if fb2.IsFB2(data) {
		format = model.FormatFB2
		t, a, lang, cover := fb2.Meta(data)
		meta.Title, meta.Authors, meta.Language, meta.Cover = t, a, lang, cover
	}
	id := fmt.Sprintf("bk-%d", time.Now().UnixNano())
	title := meta.Title
	if title == "" {
		title = strings.TrimSuffix(strings.TrimSuffix(filename, ".epub"), ".fb2")
	}
	authors := meta.Authors
	if authors == nil {
		authors = []string{}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	b := model.Book{
		ID: id, Title: title, Authors: authors, Format: format,
		Size: int64(len(data)), AddedAt: now, CoverSeed: title,
		Tags: []string{}, ShelfIDs: []string{}, FileHash: hash,
	}
	if meta.Language != "" {
		b.Language = &meta.Language
	}
	if meta.Description != "" {
		b.Description = &meta.Description
	}
	if err := st.SaveBookFile(id, data); err != nil {
		return model.Book{}, false, err
	}
	if len(meta.Cover) > 0 {
		if err := st.SaveBookCover(id, meta.Cover); err == nil && host != "" {
			u := fmt.Sprintf("http://%s/api/v1/books/%s/cover", host, id)
			b.CoverURL = &u
		}
	}
	if err := st.SaveBook(b); err != nil {
		return model.Book{}, false, err
	}
	return b, true, nil
}

// ScanWatchDir imports new .epub/.fb2 files from dir, moving processed files to .imported.
func ScanWatchDir(st *store.Store, dir string) int {
	if dir == "" {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	imported := 0
	doneDir := filepath.Join(dir, ".imported")
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(e.Name()))
		if ext != ".epub" && ext != ".fb2" {
			continue
		}
		p := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if _, created, err := importBytes(st, e.Name(), data, ""); err == nil && created {
			imported++
		}
		_ = os.MkdirAll(doneDir, 0o755)
		_ = os.Rename(p, filepath.Join(doneDir, e.Name())) // don't re-scan
	}
	return imported
}

// POST /api/v1/library/scan — import any new files in the watched folder now.
func (s *Server) scanLibrary(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"imported": ScanWatchDir(s.st, s.watchDir)})
}

func (s *Server) getBookFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok, _ := s.st.GetBook(id); !ok {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", "application/epub+zip")
	w.Header().Set("Cache-Control", "no-store")
	if data, err := s.st.BookFile(id); err == nil { // uploaded file
		// FB2 → convert to EPUB on the fly so the epub.js readers handle it.
		if fb2.IsFB2(data) {
			if epub, ok := s.fb2EPUB(id, data); ok {
				_, _ = w.Write(epub)
				return
			}
		}
		_, _ = w.Write(data)
		return
	}
	_, _ = w.Write(s.bookFile) // fallback: bundled sample
}

// fb2EPUB returns the EPUB conversion of an FB2 file, cached per book id.
func (s *Server) fb2EPUB(id string, fb2Data []byte) ([]byte, bool) {
	s.fbMu.Lock()
	defer s.fbMu.Unlock()
	if s.fbCache == nil {
		s.fbCache = map[string][]byte{}
	}
	if e, ok := s.fbCache[id]; ok {
		return e, true
	}
	epub, err := fb2.ToEPUB(fb2Data)
	if err != nil {
		return nil, false
	}
	s.fbCache[id] = epub
	return epub, true
}

func (s *Server) getBookCover(w http.ResponseWriter, r *http.Request) {
	data, err := s.st.BookCover(r.PathValue("id"))
	if err != nil {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", http.DetectContentType(data))
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

func (s *Server) listShelves(w http.ResponseWriter, r *http.Request) {
	shelves, err := s.st.ListShelves(s.currentUser(r))
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": shelves, "totalElements": len(shelves)})
}

// PATCH /api/v1/shelves/{id} { isPublic } — share a shelf with the family (public) or unshare.
func (s *Server) patchShelf(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IsPublic bool `json:"isPublic"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	sh, ok, err := s.st.SetShelfPublic(r.PathValue("id"), body.IsPublic)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, sh)
}

func (s *Server) listDevices(w http.ResponseWriter, _ *http.Request) {
	devs, err := s.st.ListDevices()
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": devs, "totalElements": len(devs)})
}

func (s *Server) deleteDevice(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteDevice(r.PathValue("id")); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) createShelf(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		badRequest(w, fmt.Errorf("name required"))
		return
	}
	sh, err := s.st.CreateShelf(strings.TrimSpace(body.Name), s.currentUser(r))
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sh)
}

func (s *Server) deleteShelf(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteShelf(r.PathValue("id")); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) addBookToShelf(w http.ResponseWriter, r *http.Request) {
	b, ok, err := s.st.SetBookShelf(r.PathValue("bookId"), r.PathValue("id"), true)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) removeBookFromShelf(w http.ResponseWriter, r *http.Request) {
	b, ok, err := s.st.SetBookShelf(r.PathValue("bookId"), r.PathValue("id"), false)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) getProgression(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok, err := s.st.GetBook(id); err != nil {
		serverError(w, err)
		return
	} else if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, s.st.GetProgression(s.currentUser(r), id))
}

func (s *Server) putProgression(w http.ResponseWriter, r *http.Request) {
	var p model.ReadProgress
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		badRequest(w, err)
		return
	}
	rp, ok, err := s.st.PutProgression(s.currentUser(r), r.PathValue("id"), p)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, rp)
}

func (s *Server) patchReadProgress(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Completed bool `json:"completed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	rp, ok, err := s.st.SetCompleted(s.currentUser(r), r.PathValue("id"), body.Completed)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, rp)
}

func (s *Server) patchRating(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Rating int `json:"rating"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	b, ok, err := s.st.SetRating(r.PathValue("id"), body.Rating)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) patchArchived(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Archived bool `json:"archived"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	b, ok, err := s.st.SetArchived(r.PathValue("id"), body.Archived)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) listHighlights(w http.ResponseWriter, r *http.Request) {
	items, err := s.st.ListHighlights(r.URL.Query().Get("bookId"))
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": items, "totalElements": len(items)})
}

func (s *Server) createHighlight(w http.ResponseWriter, r *http.Request) {
	var h model.Highlight
	if err := json.NewDecoder(r.Body).Decode(&h); err != nil {
		badRequest(w, err)
		return
	}
	if h.Color == "" {
		h.Color = "yellow"
	}
	created, err := s.st.CreateHighlight(h)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) patchHighlight(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Note  *string `json:"note"`
		Color string  `json:"color"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	h, ok, err := s.st.PatchHighlight(r.PathValue("id"), body.Note, body.Color)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) deleteHighlight(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteHighlight(r.PathValue("id")); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listBookmarks(w http.ResponseWriter, r *http.Request) {
	items, err := s.st.ListBookmarks(r.URL.Query().Get("bookId"))
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": items, "totalElements": len(items)})
}

func (s *Server) createBookmark(w http.ResponseWriter, r *http.Request) {
	var b model.Bookmark
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		badRequest(w, err)
		return
	}
	if b.Label == "" {
		b.Label = "Закладка"
	}
	created, err := s.st.CreateBookmark(b)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) deleteBookmark(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteBookmark(r.PathValue("id")); err != nil {
		serverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func notFound(w http.ResponseWriter)            { writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not Found"}) }
func badRequest(w http.ResponseWriter, e error) { writeJSON(w, http.StatusBadRequest, map[string]string{"error": e.Error()}) }
func serverError(w http.ResponseWriter, e error) {
	log.Printf("error: %v", e)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}
