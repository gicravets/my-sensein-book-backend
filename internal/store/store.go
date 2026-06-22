package store

import (
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gicravets/my-sensein-book-backend/internal/model"
	_ "modernc.org/sqlite"
)

// Store is a thin SQLite-backed store. Entities are persisted as JSON blobs to
// stay faithful to the API contract; filtering/sorting happens in Go (fine for a
// personal-scale library). Pure-Go driver (modernc.org/sqlite) keeps the binary
// static (CGO_ENABLED=0) for a tiny distroless image.
type Store struct {
	db       *sql.DB
	filesDir string
}

func Open(dbPath, filesDir string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite single-writer
	if err := os.MkdirAll(filepath.Join(filesDir, "books"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(filesDir, "covers"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{db: db, filesDir: filesDir}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	if err := s.seedIfEmpty(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) bookPath(id string) string  { return filepath.Join(s.filesDir, "books", id+".epub") }
func (s *Store) coverPath(id string) string { return filepath.Join(s.filesDir, "covers", id) }

// SaveBookFile / SaveBookCover persist uploaded bytes; BookFile / BookCover read them.
func (s *Store) SaveBookFile(id string, data []byte) error  { return os.WriteFile(s.bookPath(id), data, 0o644) }
func (s *Store) SaveBookCover(id string, data []byte) error { return os.WriteFile(s.coverPath(id), data, 0o644) }
func (s *Store) BookFile(id string) ([]byte, error)         { return os.ReadFile(s.bookPath(id)) }
func (s *Store) BookCover(id string) ([]byte, error)        { return os.ReadFile(s.coverPath(id)) }
func (s *Store) hasFile(id string) bool                     { _, err := os.Stat(s.bookPath(id)); return err == nil }

func (s *Store) SetRating(id string, rating int) (model.Book, bool, error) {
	b, ok, err := s.GetBook(id)
	if err != nil || !ok {
		return model.Book{}, ok, err
	}
	if rating < 0 {
		rating = 0
	}
	if rating > 5 {
		rating = 5
	}
	b.Rating = rating
	return b, true, s.SaveBook(b)
}

func (s *Store) SetArchived(id string, archived bool) (model.Book, bool, error) {
	b, ok, err := s.GetBook(id)
	if err != nil || !ok {
		return model.Book{}, ok, err
	}
	b.Archived = archived
	return b, true, s.SaveBook(b)
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS books     (id TEXT PRIMARY KEY, data TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS shelves   (id TEXT PRIMARY KEY, data TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS highlights(id TEXT PRIMARY KEY, book_id TEXT NOT NULL, data TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS bookmarks (id TEXT PRIMARY KEY, book_id TEXT NOT NULL, data TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS devices   (id TEXT PRIMARY KEY, name TEXT NOT NULL, key TEXT NOT NULL UNIQUE, created TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS pairings  (token TEXT PRIMARY KEY, device_key TEXT, device_name TEXT, claimed INTEGER NOT NULL DEFAULT 0, expires TEXT NOT NULL, created TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS settings  (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS users      (id TEXT PRIMARY KEY, name TEXT NOT NULL, role TEXT NOT NULL, created TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS preferences (user_id TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY(user_id, key));
		INSERT OR IGNORE INTO users(id,name,role,created) VALUES('` + OwnerID + `','owner','admin','` + time.Now().UTC().Format(time.RFC3339) + `');
	`)
	return err
}

// ---------- identity (Wave 0: minimal — every key resolves to the single owner) ----------

// OwnerID is the default single-owner user id. Per-user facets are scoped by user id
// from day one so full multi-user later is incremental, not a rewrite.
const OwnerID = "u-owner"

// UserForKey resolves an API key (device/admin/master) to a user id. Today every valid
// key maps to the owner; when multi-user lands, device keys resolve to their own user.
func (s *Store) UserForKey(key string) string {
	return OwnerID
}

// ---------- preferences (per-user KV, ref: CWA CLIENT_SETTINGS_USER) ----------

// GetPreferences returns a user's reader settings as a key→raw-JSON map.
func (s *Store) GetPreferences(userID string) (map[string]json.RawMessage, error) {
	rows, err := s.db.Query(`SELECT key, value FROM preferences WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]json.RawMessage{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = json.RawMessage(v)
	}
	return out, rows.Err()
}

// PutPreferences upserts each key in prefs for the user (last-writer-wins per key).
func (s *Store) PutPreferences(userID string, prefs map[string]json.RawMessage) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for k, v := range prefs {
		if _, err := tx.Exec(`INSERT INTO preferences(user_id,key,value) VALUES(?,?,?)
			ON CONFLICT(user_id,key) DO UPDATE SET value = excluded.value`, userID, k, string(v)); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ---------- settings (key/value, ref: Komga SERVER_SETTINGS) ----------

// GetSetting reads a server setting; ok=false when the key is absent.
func (s *Store) GetSetting(key string) (value string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return value, err == nil, err
}

// SetSetting upserts a server setting.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// HasAnyDevice reports whether at least one device key exists (a proxy for "set up").
func (s *Store) HasAnyDevice() bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&n)
	return n > 0
}

// ---------- QR pairing (RemoteAuthToken pattern, ref: Calibre-Web remote-login) ----------

type Pairing struct {
	Token   string `json:"token"`
	Expires string `json:"expires"`
}

// CreatePairing makes a short-lived pending pairing token (default 5 min).
func (s *Store) CreatePairing(ttl time.Duration) (Pairing, error) {
	b := make([]byte, 16)
	_, _ = cryptorand.Read(b)
	p := Pairing{Token: hex.EncodeToString(b), Expires: time.Now().UTC().Add(ttl).Format(time.RFC3339)}
	_, err := s.db.Exec(`INSERT INTO pairings(token,expires,created) VALUES(?,?,?)`,
		p.Token, p.Expires, time.Now().UTC().Format(time.RFC3339))
	return p, err
}

// ClaimPairing exchanges a valid pending token for a new device key.
func (s *Store) ClaimPairing(token, name string) (Device, bool, error) {
	var expires string
	var claimed int
	err := s.db.QueryRow(`SELECT expires, claimed FROM pairings WHERE token = ?`, token).Scan(&expires, &claimed)
	if err == sql.ErrNoRows {
		return Device{}, false, nil
	}
	if err != nil {
		return Device{}, false, err
	}
	if claimed == 1 {
		return Device{}, false, nil // already used
	}
	if exp, e := time.Parse(time.RFC3339, expires); e == nil && time.Now().After(exp) {
		return Device{}, false, nil // expired
	}
	d, err := s.RegisterDevice(name)
	if err != nil {
		return Device{}, false, err
	}
	_, err = s.db.Exec(`UPDATE pairings SET device_key=?, device_name=?, claimed=1 WHERE token=?`, d.Key, d.Name, token)
	return d, true, err
}

// PairingStatus tells the waiting web client whether the token was claimed.
func (s *Store) PairingStatus(token string) (status string, deviceName string) {
	var claimed int
	var expires, dn string
	err := s.db.QueryRow(`SELECT claimed, expires, COALESCE(device_name,'') FROM pairings WHERE token = ?`, token).Scan(&claimed, &expires, &dn)
	if err != nil {
		return "unknown", ""
	}
	if claimed == 1 {
		return "claimed", dn
	}
	if exp, e := time.Parse(time.RFC3339, expires); e == nil && time.Now().After(exp) {
		return "expired", ""
	}
	return "pending", ""
}

// ---------- devices / API keys ----------

type Device struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Key     string `json:"key"`
	Created string `json:"created"`
}

func (s *Store) RegisterDevice(name string) (Device, error) {
	b := make([]byte, 24)
	_, _ = cryptorand.Read(b)
	d := Device{ID: "dev-" + newID(), Name: name, Key: hex.EncodeToString(b), Created: time.Now().UTC().Format(time.RFC3339)}
	_, err := s.db.Exec(`INSERT INTO devices(id,name,key,created) VALUES(?,?,?,?)`, d.ID, d.Name, d.Key, d.Created)
	return d, err
}

// ListDevices returns registered devices WITHOUT their keys (for a management UI).
func (s *Store) ListDevices() ([]Device, error) {
	rows, err := s.db.Query(`SELECT id, name, created FROM devices ORDER BY created DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Name, &d.Created); err != nil {
			return nil, err
		}
		out = append(out, d) // Key intentionally left empty
	}
	return out, rows.Err()
}

func (s *Store) DeleteDevice(id string) error {
	_, err := s.db.Exec(`DELETE FROM devices WHERE id = ?`, id)
	return err
}

func (s *Store) ValidKey(key string) bool {
	if key == "" {
		return false
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM devices WHERE key = ?`, key).Scan(&n)
	return n > 0
}

// ---------- books ----------

func (s *Store) allBooks() ([]model.Book, error) {
	rows, err := s.db.Query(`SELECT data FROM books`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Book
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var b model.Book
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

type BookQuery struct {
	Search    string
	Shelf     string
	Tag       string
	Author    string
	Series    string
	Language  string
	Publisher string
	Format    string
	Filter    string // "" | read | unread | archived | rated | downloaded | hot
	Sort      string
	Page      int
	Size      int
}

func (s *Store) ListBooks(q BookQuery) (model.Page[model.Book], error) {
	books, err := s.allBooks()
	if err != nil {
		return model.Page[model.Book]{}, err
	}
	if q.Search != "" {
		needle := strings.ToLower(q.Search)
		books = filter(books, func(b model.Book) bool {
			if strings.Contains(strings.ToLower(b.Title), needle) {
				return true
			}
			for _, a := range b.Authors {
				if strings.Contains(strings.ToLower(a), needle) {
					return true
				}
			}
			return false
		})
	}
	if q.Shelf != "" {
		books = filter(books, func(b model.Book) bool { return contains(b.ShelfIDs, q.Shelf) })
	}
	if q.Tag != "" {
		books = filter(books, func(b model.Book) bool { return contains(b.Tags, q.Tag) })
	}
	if q.Author != "" {
		books = filter(books, func(b model.Book) bool { return contains(b.Authors, q.Author) })
	}
	if q.Series != "" {
		books = filter(books, func(b model.Book) bool { return b.Series != nil && *b.Series == q.Series })
	}
	if q.Language != "" {
		books = filter(books, func(b model.Book) bool { return b.Language != nil && *b.Language == q.Language })
	}
	if q.Publisher != "" {
		books = filter(books, func(b model.Book) bool { return b.Publisher != nil && *b.Publisher == q.Publisher })
	}
	if q.Format != "" {
		books = filter(books, func(b model.Book) bool { return string(b.Format) == q.Format })
	}
	// status filter (default hides archived)
	books = filter(books, func(b model.Book) bool {
		done := b.ReadProgress != nil && b.ReadProgress.Completed
		switch q.Filter {
		case "archived":
			return b.Archived
		case "read":
			return done && !b.Archived
		case "unread":
			return !done && !b.Archived
		case "rated":
			return b.Rating > 0 && !b.Archived
		case "downloaded":
			return s.hasFile(b.ID) && !b.Archived
		case "hot":
			return b.ReadProgress != nil && !b.Archived
		default:
			return !b.Archived
		}
	})
	if q.Filter == "hot" && q.Sort == "" {
		q.Sort = "recent"
	}
	if q.Sort == "random" {
		rand.Shuffle(len(books), func(i, j int) { books[i], books[j] = books[j], books[i] })
	} else {
		sortBooks(books, q.Sort)
	}

	total := len(books)
	if q.Size <= 0 {
		q.Size = 50
	}
	start := q.Page * q.Size
	end := start + q.Size
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	content := books[start:end]
	if content == nil {
		content = []model.Book{}
	}
	return model.Page[model.Book]{Content: content, TotalElements: total, PageNumber: q.Page, Size: q.Size}, nil
}

func (s *Store) GetBook(id string) (model.Book, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT data FROM books WHERE id = ?`, id).Scan(&raw)
	if err == sql.ErrNoRows {
		return model.Book{}, false, nil
	}
	if err != nil {
		return model.Book{}, false, err
	}
	var b model.Book
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		return model.Book{}, false, err
	}
	return b, true, nil
}

func (s *Store) SaveBook(b model.Book) error {
	raw, _ := json.Marshal(b)
	_, err := s.db.Exec(`INSERT INTO books(id,data) VALUES(?,?)
		ON CONFLICT(id) DO UPDATE SET data=excluded.data`, b.ID, string(raw))
	return err
}

// PutProgression updates a book's reading position; auto-marks completed near the end.
func (s *Store) PutProgression(id string, p model.ReadProgress) (model.Book, bool, error) {
	b, ok, err := s.GetBook(id)
	if err != nil || !ok {
		return model.Book{}, ok, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	p.LastReadAt = &now
	if p.DeviceName == nil {
		dn := "Web PWA"
		p.DeviceName = &dn
	}
	if p.TotalProgression >= 0.995 {
		p.Completed = true
	}
	b.ReadProgress = &p
	return b, true, s.SaveBook(b)
}

func (s *Store) SetCompleted(id string, completed bool) (model.Book, bool, error) {
	b, ok, err := s.GetBook(id)
	if err != nil || !ok {
		return model.Book{}, ok, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rp := model.ReadProgress{LastReadAt: &now, DeviceName: strptr("Web PWA")}
	if b.ReadProgress != nil {
		rp = *b.ReadProgress
		rp.LastReadAt = &now
	}
	rp.Completed = completed
	if completed {
		rp.Progression, rp.TotalProgression = 1, 1
	}
	b.ReadProgress = &rp
	return b, true, s.SaveBook(b)
}

// ---------- shelves ----------

func (s *Store) ListShelves() ([]model.Shelf, error) {
	rows, err := s.db.Query(`SELECT data FROM shelves`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var shelves []model.Shelf
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var sh model.Shelf
		if err := json.Unmarshal([]byte(raw), &sh); err != nil {
			return nil, err
		}
		shelves = append(shelves, sh)
	}
	books, _ := s.allBooks()
	for i := range shelves {
		n := 0
		for _, b := range books {
			if contains(b.ShelfIDs, shelves[i].ID) {
				n++
			}
		}
		shelves[i].BookCount = n
	}
	sort.Slice(shelves, func(i, j int) bool { return shelves[i].Name < shelves[j].Name })
	if shelves == nil {
		shelves = []model.Shelf{}
	}
	return shelves, rows.Err()
}

func (s *Store) CreateShelf(name string) (model.Shelf, error) {
	sh := model.Shelf{ID: "sh-" + newID(), Name: name, Kind: "normal", IsPublic: false}
	raw, _ := json.Marshal(sh)
	_, err := s.db.Exec(`INSERT INTO shelves(id,data) VALUES(?,?)`, sh.ID, string(raw))
	return sh, err
}

func (s *Store) DeleteShelf(id string) error {
	if _, err := s.db.Exec(`DELETE FROM shelves WHERE id = ?`, id); err != nil {
		return err
	}
	// drop membership from all books
	books, _ := s.allBooks()
	for _, b := range books {
		if contains(b.ShelfIDs, id) {
			b.ShelfIDs = without(b.ShelfIDs, id)
			_ = s.SaveBook(b)
		}
	}
	return nil
}

func (s *Store) SetBookShelf(bookID, shelfID string, add bool) (model.Book, bool, error) {
	b, ok, err := s.GetBook(bookID)
	if err != nil || !ok {
		return model.Book{}, ok, err
	}
	if add {
		if !contains(b.ShelfIDs, shelfID) {
			b.ShelfIDs = append(b.ShelfIDs, shelfID)
		}
	} else {
		b.ShelfIDs = without(b.ShelfIDs, shelfID)
	}
	return b, true, s.SaveBook(b)
}

// ---------- highlights ----------

func (s *Store) ListHighlights(bookID string) ([]model.Highlight, error) {
	q, args := `SELECT data FROM highlights`, []any{}
	if bookID != "" {
		q += ` WHERE book_id = ?`
		args = append(args, bookID)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Highlight{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var h model.Highlight
		if err := json.Unmarshal([]byte(raw), &h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, rows.Err()
}

func (s *Store) CreateHighlight(h model.Highlight) (model.Highlight, error) {
	h.ID = "hl-" + newID()
	h.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	raw, _ := json.Marshal(h)
	_, err := s.db.Exec(`INSERT INTO highlights(id,book_id,data) VALUES(?,?,?)`, h.ID, h.BookID, string(raw))
	return h, err
}

func (s *Store) PatchHighlight(id string, note *string, color string) (model.Highlight, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT data FROM highlights WHERE id = ?`, id).Scan(&raw)
	if err == sql.ErrNoRows {
		return model.Highlight{}, false, nil
	}
	if err != nil {
		return model.Highlight{}, false, err
	}
	var h model.Highlight
	_ = json.Unmarshal([]byte(raw), &h)
	if note != nil {
		h.Note = note
	}
	if color != "" {
		h.Color = color
	}
	nb, _ := json.Marshal(h)
	_, err = s.db.Exec(`UPDATE highlights SET data=? WHERE id=?`, string(nb), id)
	return h, true, err
}

func (s *Store) DeleteHighlight(id string) error {
	_, err := s.db.Exec(`DELETE FROM highlights WHERE id = ?`, id)
	return err
}

// ---------- bookmarks ----------

func (s *Store) ListBookmarks(bookID string) ([]model.Bookmark, error) {
	q, args := `SELECT data FROM bookmarks`, []any{}
	if bookID != "" {
		q += ` WHERE book_id = ?`
		args = append(args, bookID)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Bookmark{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var b model.Bookmark
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, rows.Err()
}

func (s *Store) CreateBookmark(b model.Bookmark) (model.Bookmark, error) {
	b.ID = "bm-" + newID()
	b.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	raw, _ := json.Marshal(b)
	_, err := s.db.Exec(`INSERT INTO bookmarks(id,book_id,data) VALUES(?,?,?)`, b.ID, b.BookID, string(raw))
	return b, err
}

func (s *Store) DeleteBookmark(id string) error {
	_, err := s.db.Exec(`DELETE FROM bookmarks WHERE id = ?`, id)
	return err
}

// ---------- helpers ----------

func sortBooks(books []model.Book, key string) {
	sort.SliceStable(books, func(i, j int) bool {
		a, b := books[i], books[j]
		switch key {
		case "title":
			return a.Title < b.Title
		case "title_desc":
			return a.Title > b.Title
		case "author":
			return first(a.Authors) < first(b.Authors)
		case "author_desc":
			return first(a.Authors) > first(b.Authors)
		case "progress":
			return prog(b) < prog(a)
		case "rating":
			return a.Rating > b.Rating
		case "recent_old":
			return recent(a) < recent(b)
		case "pub": // no real pubdate stored — proxy with addedAt
			return a.AddedAt > b.AddedAt
		case "pub_desc":
			return a.AddedAt < b.AddedAt
		default: // recent
			return recent(a) > recent(b)
		}
	})
}

func prog(b model.Book) float64 {
	if b.ReadProgress == nil {
		return -1
	}
	return b.ReadProgress.TotalProgression
}
func recent(b model.Book) string {
	if b.ReadProgress != nil && b.ReadProgress.LastReadAt != nil {
		return *b.ReadProgress.LastReadAt
	}
	return b.AddedAt
}
func first(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}
func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
func without(ss []string, v string) []string {
	out := []string{}
	for _, s := range ss {
		if s != v {
			out = append(out, s)
		}
	}
	return out
}
func filter[T any](in []T, keep func(T) bool) []T {
	out := in[:0]
	for _, x := range in {
		if keep(x) {
			out = append(out, x)
		}
	}
	return out
}
func strptr(s string) *string { return &s }

var idSeq int64

func newID() string {
	idSeq++
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), idSeq)
}
