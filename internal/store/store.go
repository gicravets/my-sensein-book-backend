package store

import (
	cryptorand "crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/gicravets/my-sensein-book-backend/internal/model"
	_ "modernc.org/sqlite"
)

// Store is a thin SQLite-backed store. Entities are persisted as JSON blobs to
// stay faithful to the API contract; filtering/sorting happens in Go (fine for a
// personal-scale library). Pure-Go driver (modernc.org/sqlite) keeps the binary
// static (CGO_ENABLED=0) for a tiny distroless image.
type Store struct {
	db      *sql.DB
	storage Storage
}

func Open(dbPath string, storage Storage) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite single-writer
	s := &Store{db: db, storage: storage}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	s.ensureColumns()
	if err := s.seedIfEmpty(); err != nil {
		return nil, err
	}
	s.migrateEmbeddedProgress()
	return s, nil
}

func bookKey(id string) string  { return "books/" + id + ".epub" }
func coverKey(id string) string { return "covers/" + id }

// SaveBookFile / SaveBookCover persist uploaded bytes; BookFile / BookCover read them.
func (s *Store) SaveBookFile(id string, data []byte) error  { return s.storage.Put(bookKey(id), data) }
func (s *Store) SaveBookCover(id string, data []byte) error { return s.storage.Put(coverKey(id), data) }
func (s *Store) BookFile(id string) ([]byte, error)         { return s.storage.Get(bookKey(id)) }
func (s *Store) BookCover(id string) ([]byte, error)        { return s.storage.Get(coverKey(id)) }
func (s *Store) hasFile(id string) bool                     { return s.storage.Has(bookKey(id)) }

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
		CREATE TABLE IF NOT EXISTS progress (user_id TEXT NOT NULL, book_id TEXT NOT NULL, data TEXT NOT NULL, PRIMARY KEY(user_id, book_id));
		CREATE TABLE IF NOT EXISTS smart_shelves (id TEXT PRIMARY KEY, data TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS read_lists (id TEXT PRIMARY KEY, data TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS tombstones (book_id TEXT PRIMARY KEY, deleted_at TEXT NOT NULL);
		CREATE VIRTUAL TABLE IF NOT EXISTS books_fts USING fts5(book_id UNINDEXED, title, authors, description, tags, tokenize='unicode61 remove_diacritics 2');
		CREATE VIRTUAL TABLE IF NOT EXISTS annotations_fts USING fts5(annot_id UNINDEXED, book_id UNINDEXED, book_title UNINDEXED, disp_text UNINDEXED, disp_note UNINDEXED, text, note, tokenize='unicode61 remove_diacritics 2');
		INSERT OR IGNORE INTO users(id,name,role,created) VALUES('` + OwnerID + `','owner','admin','` + time.Now().UTC().Format(time.RFC3339) + `');
	`)
	return err
}

// ---------- identity (Wave 0: minimal — every key resolves to the single owner) ----------

// OwnerID is the default single-owner user id. Per-user facets are scoped by user id.
const OwnerID = "u-owner"

// UserForKey resolves an API key to a user id: a device key → that device's user;
// the master/admin key or an unknown key → the owner.
func (s *Store) UserForKey(key string) string {
	var uid string
	if s.db.QueryRow(`SELECT COALESCE(user_id, ?) FROM devices WHERE key = ?`, OwnerID, key).Scan(&uid) == nil && uid != "" {
		return uid
	}
	return OwnerID
}

// ensureColumns adds user_id to older device/pairing tables (idempotent migration).
func (s *Store) ensureColumns() {
	addCol := func(table, col, def string) {
		rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
		if err != nil {
			return
		}
		has := false
		for rows.Next() {
			var cid, notnull, pk int
			var name, ctype string
			var dflt any
			_ = rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk)
			if name == col {
				has = true
			}
		}
		rows.Close()
		if !has {
			_, _ = s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + def)
		}
	}
	addCol("devices", "user_id", "TEXT NOT NULL DEFAULT '"+OwnerID+"'")
	addCol("pairings", "user_id", "TEXT NOT NULL DEFAULT '"+OwnerID+"'")
}

// ---------- users (shared/family: users own devices; per-user state scoped by user id) ----------

type User struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	Created string `json:"created"`
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, name, role, created FROM users ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		if rows.Scan(&u.ID, &u.Name, &u.Role, &u.Created) == nil {
			out = append(out, u)
		}
	}
	return out, rows.Err()
}

func (s *Store) CreateUser(name string) (User, error) {
	u := User{ID: "u-" + newID(), Name: name, Role: "member", Created: time.Now().UTC().Format(time.RFC3339)}
	_, err := s.db.Exec(`INSERT INTO users(id,name,role,created) VALUES(?,?,?,?)`, u.ID, u.Name, u.Role, u.Created)
	return u, err
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

// CreatePairing makes a short-lived pending pairing token (default 5 min) for a user.
func (s *Store) CreatePairing(ttl time.Duration, userID string) (Pairing, error) {
	if userID == "" {
		userID = OwnerID
	}
	b := make([]byte, 16)
	_, _ = cryptorand.Read(b)
	p := Pairing{Token: hex.EncodeToString(b), Expires: time.Now().UTC().Add(ttl).Format(time.RFC3339)}
	_, err := s.db.Exec(`INSERT INTO pairings(token,expires,created,user_id) VALUES(?,?,?,?)`,
		p.Token, p.Expires, time.Now().UTC().Format(time.RFC3339), userID)
	return p, err
}

// ClaimPairing exchanges a valid pending token for a new device key (joined to the pairing's user).
func (s *Store) ClaimPairing(token, name string) (Device, bool, error) {
	var expires, userID string
	var claimed int
	err := s.db.QueryRow(`SELECT expires, claimed, COALESCE(user_id, ?) FROM pairings WHERE token = ?`, OwnerID, token).Scan(&expires, &claimed, &userID)
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
	d, err := s.RegisterDevice(name, userID)
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
	UserID  string `json:"userId,omitempty"`
}

func (s *Store) RegisterDevice(name, userID string) (Device, error) {
	if userID == "" {
		userID = OwnerID
	}
	b := make([]byte, 24)
	_, _ = cryptorand.Read(b)
	d := Device{ID: "dev-" + newID(), Name: name, Key: hex.EncodeToString(b), Created: time.Now().UTC().Format(time.RFC3339), UserID: userID}
	_, err := s.db.Exec(`INSERT INTO devices(id,name,key,created,user_id) VALUES(?,?,?,?,?)`, d.ID, d.Name, d.Key, d.Created, userID)
	return d, err
}

// ListDevices returns registered devices WITHOUT their keys (for a management UI).
func (s *Store) ListDevices() ([]Device, error) {
	rows, err := s.db.Query(`SELECT id, name, created, COALESCE(user_id,'') FROM devices ORDER BY created DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Name, &d.Created, &d.UserID); err != nil {
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
	Search    string `json:"search,omitempty"`
	Shelf     string `json:"shelf,omitempty"`
	Tag       string `json:"tag,omitempty"`
	Author    string `json:"author,omitempty"`
	Series    string `json:"series,omitempty"`
	Language  string `json:"language,omitempty"`
	Publisher string `json:"publisher,omitempty"`
	Format    string `json:"format,omitempty"`
	Filter    string `json:"filter,omitempty"` // "" | read | unread | archived | rated | downloaded | hot
	Sort      string `json:"sort,omitempty"`
	Page      int    `json:"-"`
	Size      int    `json:"-"`
}

func (s *Store) ListBooks(userID string, q BookQuery) (model.Page[model.Book], error) {
	books, err := s.allBooks()
	if err != nil {
		return model.Page[model.Book]{}, err
	}
	// overlay the requesting user's reading progress (per-user state)
	prog := s.userProgressMap(userID)
	for i := range books {
		books[i].ReadProgress = prog[books[i].ID]
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
	b.UpdatedAt = time.Now().UTC().Format(syncTimeFmt) // touch for the sync delta
	raw, _ := json.Marshal(b)
	_, err := s.db.Exec(`INSERT INTO books(id,data) VALUES(?,?)
		ON CONFLICT(id) DO UPDATE SET data=excluded.data`, b.ID, string(raw))
	return err
}

// syncTimeFmt is fixed-width (9 fractional digits) so string/SQL "since" comparison is correct.
const syncTimeFmt = "2006-01-02T15:04:05.000000000Z"

// FindBookByHash returns a book with the given file hash, if any (upload dedup).
func (s *Store) FindBookByHash(hash string) (model.Book, bool) {
	if hash == "" {
		return model.Book{}, false
	}
	books, _ := s.allBooks()
	for _, b := range books {
		if b.FileHash == hash {
			return b, true
		}
	}
	return model.Book{}, false
}

// SoftDeleteBook removes a book and records a tombstone so the deletion syncs to clients.
func (s *Store) SoftDeleteBook(id string) (bool, error) {
	if _, ok, _ := s.GetBook(id); !ok {
		return false, nil
	}
	now := time.Now().UTC().Format(syncTimeFmt)
	if _, err := s.db.Exec(`INSERT INTO tombstones(book_id,deleted_at) VALUES(?,?)
		ON CONFLICT(book_id) DO UPDATE SET deleted_at=excluded.deleted_at`, id, now); err != nil {
		return false, err
	}
	if _, err := s.db.Exec(`DELETE FROM books WHERE id = ?`, id); err != nil {
		return false, err
	}
	_ = s.storage.Delete(bookKey(id))
	_ = s.storage.Delete(coverKey(id))
	return true, nil
}

// SyncDelta returns books changed since `since` (RFC3339; empty = all) plus the ids
// deleted since then, and the server time to use as the next sync point. (ref: Komga SYNC_POINT)
func (s *Store) SyncDelta(since string) (changed []model.Book, removed []string, serverTime string, err error) {
	serverTime = time.Now().UTC().Format(syncTimeFmt)
	books, err := s.allBooks()
	if err != nil {
		return nil, nil, serverTime, err
	}
	changed = []model.Book{}
	for _, b := range books {
		if since == "" || b.UpdatedAt == "" || b.UpdatedAt > since {
			changed = append(changed, b)
		}
	}
	removed = []string{}
	if since != "" {
		rows, e := s.db.Query(`SELECT book_id FROM tombstones WHERE deleted_at > ?`, since)
		if e != nil {
			return changed, removed, serverTime, e
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				removed = append(removed, id)
			}
		}
	}
	return changed, removed, serverTime, nil
}

// PutProgression updates a book's reading position; auto-marks completed near the end.
// ---------- per-user reading progress (each family member has their own place) ----------

// GetProgression returns a user's reading progress for a book (nil if none).
func (s *Store) GetProgression(userID, bookID string) *model.ReadProgress {
	var raw string
	if s.db.QueryRow(`SELECT data FROM progress WHERE user_id=? AND book_id=?`, userID, bookID).Scan(&raw) != nil {
		return nil
	}
	var rp model.ReadProgress
	if json.Unmarshal([]byte(raw), &rp) != nil {
		return nil
	}
	return &rp
}

func (s *Store) saveProgress(userID, bookID string, rp model.ReadProgress) error {
	raw, _ := json.Marshal(rp)
	_, err := s.db.Exec(`INSERT INTO progress(user_id,book_id,data) VALUES(?,?,?)
		ON CONFLICT(user_id,book_id) DO UPDATE SET data=excluded.data`, userID, bookID, string(raw))
	return err
}

// userProgressMap is every book's progress for one user (for the ListBooks overlay).
func (s *Store) userProgressMap(userID string) map[string]*model.ReadProgress {
	out := map[string]*model.ReadProgress{}
	rows, err := s.db.Query(`SELECT book_id, data FROM progress WHERE user_id = ?`, userID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var bid, raw string
		if rows.Scan(&bid, &raw) == nil {
			var rp model.ReadProgress
			if json.Unmarshal([]byte(raw), &rp) == nil {
				out[bid] = &rp
			}
		}
	}
	return out
}

// migrateEmbeddedProgress moves any progress embedded in book JSON to the owner's
// per-user rows, once (so existing/seed reading state isn't lost).
func (s *Store) migrateEmbeddedProgress() {
	if _, done, _ := s.GetSetting("progress_migrated"); done {
		return
	}
	books, _ := s.allBooks()
	for _, b := range books {
		if b.ReadProgress != nil {
			_ = s.saveProgress(OwnerID, b.ID, *b.ReadProgress)
		}
	}
	_ = s.SetSetting("progress_migrated", "true")
}

func (s *Store) PutProgression(userID, id string, p model.ReadProgress) (*model.ReadProgress, bool, error) {
	if _, ok, err := s.GetBook(id); err != nil || !ok {
		return nil, ok, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	p.LastReadAt = &now
	if p.DeviceName == nil {
		dn := "client"
		p.DeviceName = &dn
	}
	if p.TotalProgression >= 0.995 {
		p.Completed = true
	}
	return &p, true, s.saveProgress(userID, id, p)
}

func (s *Store) SetCompleted(userID, id string, completed bool) (*model.ReadProgress, bool, error) {
	if _, ok, err := s.GetBook(id); err != nil || !ok {
		return nil, ok, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rp := model.ReadProgress{LastReadAt: &now, DeviceName: strptr("client")}
	if cur := s.GetProgression(userID, id); cur != nil {
		rp = *cur
		rp.LastReadAt = &now
	}
	rp.Completed = completed
	if completed {
		rp.Progression, rp.TotalProgression = 1, 1
	}
	return &rp, true, s.saveProgress(userID, id, rp)
}

// ---------- shelves ----------

// ListShelves returns shelves visible to a user: their own + public + legacy shared ("" owner).
func (s *Store) ListShelves(userID string) ([]model.Shelf, error) {
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
		if sh.OwnerID == "" || sh.OwnerID == userID || sh.IsPublic {
			shelves = append(shelves, sh)
		}
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

func (s *Store) CreateShelf(name, ownerID string) (model.Shelf, error) {
	if ownerID == "" {
		ownerID = OwnerID
	}
	sh := model.Shelf{ID: "sh-" + newID(), Name: name, Kind: "normal", IsPublic: false, OwnerID: ownerID}
	raw, _ := json.Marshal(sh)
	_, err := s.db.Exec(`INSERT INTO shelves(id,data) VALUES(?,?)`, sh.ID, string(raw))
	return sh, err
}

// SetShelfPublic toggles a shelf's public (shared-with-family) flag.
func (s *Store) SetShelfPublic(id string, public bool) (model.Shelf, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT data FROM shelves WHERE id = ?`, id).Scan(&raw)
	if err == sql.ErrNoRows {
		return model.Shelf{}, false, nil
	}
	if err != nil {
		return model.Shelf{}, false, err
	}
	var sh model.Shelf
	if err := json.Unmarshal([]byte(raw), &sh); err != nil {
		return model.Shelf{}, false, err
	}
	sh.IsPublic = public
	out, _ := json.Marshal(sh)
	_, err = s.db.Exec(`UPDATE shelves SET data = ? WHERE id = ?`, string(out), id)
	return sh, true, err
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

// ---------- read lists (ordered, cross-series; ref: Komga READLIST) ----------

type ReadList struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	BookIDs []string `json:"bookIds"`
}

func (s *Store) ListReadLists() ([]ReadList, error) {
	rows, err := s.db.Query(`SELECT data FROM read_lists`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReadList{}
	for rows.Next() {
		var raw string
		if rows.Scan(&raw) == nil {
			var rl ReadList
			if json.Unmarshal([]byte(raw), &rl) == nil {
				out = append(out, rl)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, rows.Err()
}

func (s *Store) getReadList(id string) (ReadList, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT data FROM read_lists WHERE id = ?`, id).Scan(&raw)
	if err == sql.ErrNoRows {
		return ReadList{}, false, nil
	}
	if err != nil {
		return ReadList{}, false, err
	}
	var rl ReadList
	return rl, json.Unmarshal([]byte(raw), &rl) == nil, nil
}

func (s *Store) saveReadList(rl ReadList) error {
	raw, _ := json.Marshal(rl)
	_, err := s.db.Exec(`INSERT INTO read_lists(id,data) VALUES(?,?)
		ON CONFLICT(id) DO UPDATE SET data = excluded.data`, rl.ID, string(raw))
	return err
}

func (s *Store) CreateReadList(name string) (ReadList, error) {
	rl := ReadList{ID: "rl-" + newID(), Name: name, BookIDs: []string{}}
	return rl, s.saveReadList(rl)
}

func (s *Store) DeleteReadList(id string) error {
	_, err := s.db.Exec(`DELETE FROM read_lists WHERE id = ?`, id)
	return err
}

// AddToReadList appends a book (kept ordered, no duplicates).
func (s *Store) AddToReadList(id, bookID string) (bool, error) {
	rl, ok, err := s.getReadList(id)
	if err != nil || !ok {
		return ok, err
	}
	if !contains(rl.BookIDs, bookID) {
		rl.BookIDs = append(rl.BookIDs, bookID)
	}
	return true, s.saveReadList(rl)
}

func (s *Store) RemoveFromReadList(id, bookID string) (bool, error) {
	rl, ok, err := s.getReadList(id)
	if err != nil || !ok {
		return ok, err
	}
	rl.BookIDs = without(rl.BookIDs, bookID)
	return true, s.saveReadList(rl)
}

// ReadListBooks returns the list's books in order, with the user's progress overlaid.
func (s *Store) ReadListBooks(userID, id string) ([]model.Book, bool, error) {
	rl, ok, err := s.getReadList(id)
	if err != nil || !ok {
		return nil, ok, err
	}
	prog := s.userProgressMap(userID)
	out := []model.Book{}
	for _, bid := range rl.BookIDs {
		if b, found, _ := s.GetBook(bid); found {
			b.ReadProgress = prog[b.ID]
			out = append(out, b)
		}
	}
	return out, true, nil
}

// ---------- series (multi-volume grouping; ref: Komga SERIES) ----------

type SeriesInfo struct {
	Name      string `json:"name"`
	BookCount int    `json:"bookCount"`
	CoverSeed string `json:"coverSeed,omitempty"`
}

// ListSeries groups the library's books by their Series metadata (cover = first volume).
func (s *Store) ListSeries() ([]SeriesInfo, error) {
	books, err := s.allBooks()
	if err != nil {
		return nil, err
	}
	sort.Slice(books, func(i, j int) bool {
		bi, bj := 0.0, 0.0
		if books[i].SeriesIndex != nil {
			bi = *books[i].SeriesIndex
		}
		if books[j].SeriesIndex != nil {
			bj = *books[j].SeriesIndex
		}
		return bi < bj
	})
	idx := map[string]*SeriesInfo{}
	order := []string{}
	for _, b := range books {
		if b.Series == nil || *b.Series == "" {
			continue
		}
		name := *b.Series
		if idx[name] == nil {
			idx[name] = &SeriesInfo{Name: name, CoverSeed: b.CoverSeed}
			order = append(order, name)
		}
		idx[name].BookCount++
	}
	out := []SeriesInfo{}
	for _, n := range order {
		out = append(out, *idx[n])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ---------- smart shelves (dynamic, rule-based; ref: CWA magic_shelf rules JSON) ----------

// SmartShelf is a saved query: its books are computed by running Rules through ListBooks.
type SmartShelf struct {
	ID    string    `json:"id"`
	Name  string    `json:"name"`
	Rules BookQuery `json:"rules"`
}

func (s *Store) ListSmartShelves() ([]SmartShelf, error) {
	rows, err := s.db.Query(`SELECT data FROM smart_shelves`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SmartShelf{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var sh SmartShelf
		if json.Unmarshal([]byte(raw), &sh) == nil {
			out = append(out, sh)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, rows.Err()
}

func (s *Store) CreateSmartShelf(name string, rules BookQuery) (SmartShelf, error) {
	rules.Page, rules.Size = 0, 0 // paging is per-request, not part of the rule
	sh := SmartShelf{ID: "ss-" + newID(), Name: name, Rules: rules}
	raw, _ := json.Marshal(sh)
	_, err := s.db.Exec(`INSERT INTO smart_shelves(id,data) VALUES(?,?)`, sh.ID, string(raw))
	return sh, err
}

func (s *Store) DeleteSmartShelf(id string) error {
	_, err := s.db.Exec(`DELETE FROM smart_shelves WHERE id = ?`, id)
	return err
}

// SmartShelfBooks evaluates a smart shelf's rules (with request paging) into a page of books.
func (s *Store) SmartShelfBooks(userID, id string, page, size int) (model.Page[model.Book], bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT data FROM smart_shelves WHERE id = ?`, id).Scan(&raw)
	if err == sql.ErrNoRows {
		return model.Page[model.Book]{}, false, nil
	}
	if err != nil {
		return model.Page[model.Book]{}, false, err
	}
	var sh SmartShelf
	if err := json.Unmarshal([]byte(raw), &sh); err != nil {
		return model.Page[model.Book]{}, false, err
	}
	q := sh.Rules
	q.Page, q.Size = page, size
	res, err := s.ListBooks(userID, q)
	return res, true, err
}

// ---------- full-text search (SQLite FTS5; unicode61 remove_diacritics folds ё/е) ----------

// HighlightHit is a saved quote matched by search.
type HighlightHit struct {
	ID        string `json:"id"`
	BookID    string `json:"bookId"`
	BookTitle string `json:"bookTitle"`
	Text      string `json:"text"`
	Note      string `json:"note"`
}

// SearchResults bundles book and annotation matches.
type SearchResults struct {
	Books      []model.Book   `json:"books"`
	Highlights []HighlightHit `json:"highlights"`
}

// Search runs a full-text query over book metadata and saved highlights. The FTS index
// is rebuilt from current data per query (cheap for a personal/family library).
func (s *Store) Search(q string, limit int) (SearchResults, error) {
	out := SearchResults{Books: []model.Book{}, Highlights: []HighlightHit{}}
	match := ftsMatch(q)
	if match == "" {
		return out, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if err := s.refreshFTS(); err != nil {
		return out, err
	}

	// Collect ids first, then load books — never query while a cursor holds the
	// single sqlite connection (MaxOpenConns(1)), or GetBook would deadlock.
	var ids []string
	brows, err := s.db.Query(`SELECT book_id FROM books_fts WHERE books_fts MATCH ? ORDER BY rank LIMIT ?`, match, limit)
	if err != nil {
		return out, err
	}
	for brows.Next() {
		var id string
		if brows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	brows.Close()
	for _, id := range ids {
		if b, ok, _ := s.GetBook(id); ok {
			out.Books = append(out.Books, b)
		}
	}

	hrows, err := s.db.Query(`SELECT annot_id, book_id, book_title, disp_text, disp_note FROM annotations_fts WHERE annotations_fts MATCH ? ORDER BY rank LIMIT ?`, match, limit)
	if err != nil {
		return out, err
	}
	defer hrows.Close()
	for hrows.Next() {
		var h HighlightHit
		if hrows.Scan(&h.ID, &h.BookID, &h.BookTitle, &h.Text, &h.Note) == nil {
			out.Highlights = append(out.Highlights, h)
		}
	}
	return out, nil
}

func (s *Store) refreshFTS() error {
	books, err := s.allBooks()
	if err != nil {
		return err
	}
	hls, err := s.allHighlights()
	if err != nil {
		return err
	}
	title := map[string]string{}
	for _, b := range books {
		title[b.ID] = b.Title
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM books_fts; DELETE FROM annotations_fts;`); err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, b := range books {
		desc, pub := "", ""
		if b.Description != nil {
			desc = *b.Description
		}
		if b.Publisher != nil {
			pub = *b.Publisher
		}
		if _, err := tx.Exec(`INSERT INTO books_fts(book_id,title,authors,description,tags) VALUES(?,?,?,?,?)`,
			b.ID, foldRU(b.Title), foldRU(strings.Join(b.Authors, " ")), foldRU(desc), foldRU(strings.Join(b.Tags, " ")+" "+pub)); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	for _, h := range hls {
		note := ""
		if h.Note != nil {
			note = *h.Note
		}
		if _, err := tx.Exec(`INSERT INTO annotations_fts(annot_id,book_id,book_title,disp_text,disp_note,text,note) VALUES(?,?,?,?,?,?,?)`,
			h.ID, h.BookID, title[h.BookID], h.Text, note, foldRU(h.Text), foldRU(note)); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) allHighlights() ([]model.Highlight, error) {
	rows, err := s.db.Query(`SELECT data FROM highlights`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Highlight{}
	for rows.Next() {
		var raw string
		if rows.Scan(&raw) == nil {
			var h model.Highlight
			if json.Unmarshal([]byte(raw), &h) == nil {
				out = append(out, h)
			}
		}
	}
	return out, rows.Err()
}

// foldRU folds Russian ё→е (FTS5 remove_diacritics doesn't fold Cyrillic in this build),
// applied to both indexed text and queries so ёлка ↔ елка match either way.
func foldRU(s string) string { return ruFolder.Replace(s) }

var ruFolder = strings.NewReplacer("ё", "е", "Ё", "Е")

// ftsMatch builds a prefix AND query from free text (letters/digits only, each term + '*').
func ftsMatch(q string) string {
	q = foldRU(q)
	var terms []string
	for _, t := range strings.Fields(q) {
		var sb strings.Builder
		for _, r := range t {
			if unicode.IsLetter(r) || unicode.IsNumber(r) {
				sb.WriteRune(r)
			}
		}
		if sb.Len() > 0 {
			terms = append(terms, sb.String()+"*")
		}
	}
	return strings.Join(terms, " ")
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
