package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gicravets/my-sensein-book-backend/internal/store"
)

func newTestServer(t *testing.T, cfg Config) http.Handler {
	t.Helper()
	tmp := t.TempDir()
	st, err := store.Open(filepath.Join(tmp, "t.sqlite"), filepath.Join(tmp, "files"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewRouter(st, cfg)
}

func do(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func doKey(t *testing.T, h http.Handler, method, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(method, path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if key != "" {
		r.Header.Set("X-API-Key", key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestUsersAndPerUserPrefs(t *testing.T) {
	h := newTestServer(t, Config{})

	// a device with no userId joins the owner
	var d1 struct {
		Key    string `json:"key"`
		UserID string `json:"userId"`
	}
	json.Unmarshal(do(t, h, "POST", "/api/v1/auth/device", map[string]any{"name": "owner-phone"}).Body.Bytes(), &d1)
	if d1.Key == "" || d1.UserID != "u-owner" {
		t.Fatalf("owner device = %+v", d1)
	}

	// create a family user + a device that joins them
	var u struct {
		ID string `json:"id"`
	}
	json.Unmarshal(do(t, h, "POST", "/api/v1/users", map[string]any{"name": "Аня"}).Body.Bytes(), &u)
	if u.ID == "" {
		t.Fatal("no user id")
	}
	var d2 struct {
		Key    string `json:"key"`
		UserID string `json:"userId"`
	}
	json.Unmarshal(do(t, h, "POST", "/api/v1/auth/device", map[string]any{"name": "anya-phone", "userId": u.ID}).Body.Bytes(), &d2)
	if d2.Key == "" || d2.UserID != u.ID {
		t.Fatalf("anya device = %+v", d2)
	}

	// per-user preferences are isolated by the device's user
	doKey(t, h, "PUT", "/api/v1/preferences", d1.Key, map[string]any{"theme": "night"})
	doKey(t, h, "PUT", "/api/v1/preferences", d2.Key, map[string]any{"theme": "sepia"})
	var p1, p2 map[string]json.RawMessage
	json.Unmarshal(doKey(t, h, "GET", "/api/v1/preferences", d1.Key, nil).Body.Bytes(), &p1)
	json.Unmarshal(doKey(t, h, "GET", "/api/v1/preferences", d2.Key, nil).Body.Bytes(), &p2)
	if string(p1["theme"]) != `"night"` {
		t.Errorf("owner theme = %s want night", p1["theme"])
	}
	if string(p2["theme"]) != `"sepia"` {
		t.Errorf("anya theme = %s want sepia", p2["theme"])
	}
}

func TestPerUserProgress(t *testing.T) {
	h := newTestServer(t, Config{})

	// two fresh users (not the owner, who has migrated seed progress), each with a device
	mkUser := func(name string) (userID, key string) {
		var u struct {
			ID string `json:"id"`
		}
		json.Unmarshal(do(t, h, "POST", "/api/v1/users", map[string]any{"name": name}).Body.Bytes(), &u)
		var d struct {
			Key string `json:"key"`
		}
		json.Unmarshal(do(t, h, "POST", "/api/v1/auth/device", map[string]any{"name": name + "-dev", "userId": u.ID}).Body.Bytes(), &d)
		return u.ID, d.Key
	}
	_, ka := mkUser("Папа")
	_, kb := mkUser("Аня")

	var page struct {
		Content []struct {
			ID string `json:"id"`
		} `json:"content"`
	}
	json.Unmarshal(doKey(t, h, "GET", "/api/v1/books", ka, nil).Body.Bytes(), &page)
	if len(page.Content) == 0 {
		t.Fatal("no seed books")
	}
	bid := page.Content[0].ID

	// user A records a reading position
	doKey(t, h, "PUT", "/api/v1/books/"+bid+"/progression", ka, map[string]any{"totalProgression": 0.5})

	// A sees their progress; B does not (isolation)
	var pa, pb map[string]any
	json.Unmarshal(doKey(t, h, "GET", "/api/v1/books/"+bid+"/progression", ka, nil).Body.Bytes(), &pa)
	if pa["totalProgression"] != 0.5 {
		t.Errorf("A progression = %v want 0.5", pa["totalProgression"])
	}
	json.Unmarshal(doKey(t, h, "GET", "/api/v1/books/"+bid+"/progression", kb, nil).Body.Bytes(), &pb)
	if pb["totalProgression"] == 0.5 {
		t.Errorf("B leaked A's progress")
	}

	// list overlay is per-user too
	var la struct {
		Content []struct {
			ID           string          `json:"id"`
			ReadProgress map[string]any  `json:"readProgress"`
		} `json:"content"`
	}
	json.Unmarshal(doKey(t, h, "GET", "/api/v1/books", ka, nil).Body.Bytes(), &la)
	for _, bk := range la.Content {
		if bk.ID == bid && (bk.ReadProgress == nil || bk.ReadProgress["totalProgression"] != 0.5) {
			t.Errorf("A list overlay missing progress for %s: %v", bid, bk.ReadProgress)
		}
	}
}

func TestShelfSharing(t *testing.T) {
	h := newTestServer(t, Config{})
	mk := func(name string) string {
		var u struct {
			ID string `json:"id"`
		}
		json.Unmarshal(do(t, h, "POST", "/api/v1/users", map[string]any{"name": name}).Body.Bytes(), &u)
		var d struct {
			Key string `json:"key"`
		}
		json.Unmarshal(do(t, h, "POST", "/api/v1/auth/device", map[string]any{"name": name + "-d", "userId": u.ID}).Body.Bytes(), &d)
		return d.Key
	}
	ka, kb := mk("A"), mk("B")

	var sh struct {
		ID string `json:"id"`
	}
	json.Unmarshal(doKey(t, h, "POST", "/api/v1/shelves", ka, map[string]any{"name": "A private"}).Body.Bytes(), &sh)
	if sh.ID == "" {
		t.Fatal("no shelf id")
	}

	has := func(key, id string) bool {
		var l struct {
			Content []struct {
				ID string `json:"id"`
			} `json:"content"`
		}
		json.Unmarshal(doKey(t, h, "GET", "/api/v1/shelves", key, nil).Body.Bytes(), &l)
		for _, s := range l.Content {
			if s.ID == id {
				return true
			}
		}
		return false
	}
	if !has(ka, sh.ID) {
		t.Error("A cannot see own shelf")
	}
	if has(kb, sh.ID) {
		t.Error("B sees A's private shelf")
	}

	// A shares it (public) -> B sees it
	doKey(t, h, "PATCH", "/api/v1/shelves/"+sh.ID, ka, map[string]any{"isPublic": true})
	if !has(kb, sh.ID) {
		t.Error("B cannot see A's public shelf")
	}
}

func TestPreferencesSync(t *testing.T) {
	h := newTestServer(t, Config{Version: "v1.0.0"})

	w := do(t, h, "GET", "/api/v1/preferences", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET prefs: %d", w.Code)
	}
	var empty map[string]json.RawMessage
	if json.Unmarshal(w.Body.Bytes(), &empty); len(empty) != 0 {
		t.Errorf("fresh prefs not empty: %v", empty)
	}

	do(t, h, "PUT", "/api/v1/preferences", map[string]any{"theme": "night", "fontPct": 18})
	do(t, h, "PUT", "/api/v1/preferences", map[string]any{"readingMode": "curl", "fontPct": 20}) // merge per key

	w = do(t, h, "GET", "/api/v1/preferences", nil)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got["theme"]) != `"night"` {
		t.Errorf("theme = %s want \"night\"", got["theme"])
	}
	if string(got["fontPct"]) != "20" {
		t.Errorf("fontPct = %s want 20 (last write wins)", got["fontPct"])
	}
	if string(got["readingMode"]) != `"curl"` {
		t.Errorf("readingMode = %s want \"curl\"", got["readingMode"])
	}
}

func TestDemoReadOnly(t *testing.T) {
	h := newTestServer(t, Config{Demo: true})
	if w := do(t, h, "GET", "/api/v1/books", nil); w.Code != http.StatusOK {
		t.Errorf("demo GET books = %d want 200", w.Code)
	}
	if w := do(t, h, "PUT", "/api/v1/preferences", map[string]any{"theme": "x"}); w.Code != http.StatusForbidden {
		t.Errorf("demo PUT prefs = %d want 403", w.Code)
	}
	if w := do(t, h, "POST", "/api/v1/shelves", map[string]any{"name": "x"}); w.Code != http.StatusForbidden {
		t.Errorf("demo POST shelves = %d want 403", w.Code)
	}
}

func TestSmartShelves(t *testing.T) {
	h := newTestServer(t, Config{})

	// create a rule-based shelf
	w := do(t, h, "POST", "/api/v1/smart-shelves", map[string]any{
		"name": "Unread", "rules": map[string]any{"filter": "unread"},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d want 201", w.Code)
	}
	var created struct{ ID, Name string }
	json.Unmarshal(w.Body.Bytes(), &created)
	if created.ID == "" || created.Name != "Unread" {
		t.Fatalf("created = %+v", created)
	}

	// list
	var list struct {
		Content       []map[string]any `json:"content"`
		TotalElements int              `json:"totalElements"`
	}
	json.Unmarshal(do(t, h, "GET", "/api/v1/smart-shelves", nil).Body.Bytes(), &list)
	if list.TotalElements != 1 {
		t.Errorf("list total = %d want 1", list.TotalElements)
	}

	// evaluate -> a page of books
	w = do(t, h, "GET", "/api/v1/smart-shelves/"+created.ID+"/books", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("eval = %d want 200", w.Code)
	}
	var page struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("eval decode: %v", err)
	}

	// missing shelf -> 404
	if w := do(t, h, "GET", "/api/v1/smart-shelves/ss-nope/books", nil); w.Code != http.StatusNotFound {
		t.Errorf("missing eval = %d want 404", w.Code)
	}

	// delete -> empty
	if w := do(t, h, "DELETE", "/api/v1/smart-shelves/"+created.ID, nil); w.Code != http.StatusNoContent {
		t.Errorf("delete = %d want 204", w.Code)
	}
	json.Unmarshal(do(t, h, "GET", "/api/v1/smart-shelves", nil).Body.Bytes(), &list)
	if list.TotalElements != 0 {
		t.Errorf("after delete total = %d want 0", list.TotalElements)
	}
}

func TestSearch(t *testing.T) {
	h := newTestServer(t, Config{})
	var res struct {
		Books      []map[string]any `json:"books"`
		Highlights []map[string]any `json:"highlights"`
	}

	// metadata: the seed library has "Pride and Prejudice" by Jane Austen
	json.Unmarshal(do(t, h, "GET", "/api/v1/search?q=prejud", nil).Body.Bytes(), &res)
	if len(res.Books) == 0 {
		t.Errorf("expected a book match for 'prejud'")
	}
	json.Unmarshal(do(t, h, "GET", "/api/v1/search?q=austen", nil).Body.Bytes(), &res)
	if len(res.Books) == 0 {
		t.Errorf("expected an author match for 'austen'")
	}

	// annotation match + diacritic folding (ё -> е)
	var page struct {
		Content []struct {
			ID string `json:"id"`
		} `json:"content"`
	}
	json.Unmarshal(do(t, h, "GET", "/api/v1/books", nil).Body.Bytes(), &page)
	if len(page.Content) == 0 {
		t.Fatal("no seed books")
	}
	cr := do(t, h, "POST", "/api/v1/highlights", map[string]any{
		"bookId": page.Content[0].ID, "text": "xyzzy ёлка", "color": "yellow",
		"locator": map[string]any{"type": "cfi", "value": "x", "progression": 0},
	})
	if cr.Code != http.StatusCreated && cr.Code != http.StatusOK {
		t.Fatalf("create highlight = %d", cr.Code)
	}
	// indexing: plain token must match
	json.Unmarshal(do(t, h, "GET", "/api/v1/search?q=xyzzy", nil).Body.Bytes(), &res)
	if len(res.Highlights) == 0 {
		t.Errorf("expected highlight match for plain token 'xyzzy' (annotation indexing)")
	}
	// ё/е folding: query "елка" matches highlight "ёлка"; display keeps the original
	json.Unmarshal(do(t, h, "GET", "/api/v1/search?q=елка", nil).Body.Bytes(), &res)
	if len(res.Highlights) == 0 {
		t.Errorf("expected ё/е-folded highlight match (елка ~ ёлка)")
	} else if txt, _ := res.Highlights[0]["text"].(string); txt != "xyzzy ёлка" {
		t.Errorf("display text folded; got %q want original 'xyzzy ёлка'", txt)
	}
}

func TestSetupClaim(t *testing.T) {
	h := newTestServer(t, Config{})

	var st map[string]any
	json.Unmarshal(do(t, h, "GET", "/api/v1/setup", nil).Body.Bytes(), &st)
	if st["claimed"] != false {
		t.Errorf("fresh claimed = %v want false", st["claimed"])
	}
	if w := do(t, h, "POST", "/api/v1/setup/claim", map[string]any{}); w.Code != http.StatusCreated {
		t.Fatalf("claim = %d want 201", w.Code)
	}
	json.Unmarshal(do(t, h, "GET", "/api/v1/setup", nil).Body.Bytes(), &st)
	if st["claimed"] != true {
		t.Errorf("after claim = %v want true", st["claimed"])
	}
	if w := do(t, h, "POST", "/api/v1/setup/claim", map[string]any{}); w.Code != http.StatusConflict {
		t.Errorf("re-claim = %d want 409", w.Code)
	}
}
