package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func uploadBook(t *testing.T, h http.Handler, name string, content []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(content)
	_ = mw.Close()
	r := httptest.NewRequest("POST", "/api/v1/books", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestEnrich(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"docs":[{"cover_i":42,"first_sentence":["A canned first sentence."]}]}`))
	}))
	defer mock.Close()

	h := newTestServer(t, Config{MetaBase: mock.URL})
	w := do(t, h, "POST", "/api/v1/books/bk-1/enrich", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("enrich = %d want 200", w.Code)
	}
	var res struct {
		Enriched bool `json:"enriched"`
		Book     struct {
			Description *string `json:"description"`
			CoverURL    *string `json:"coverUrl"`
		} `json:"book"`
	}
	json.Unmarshal(w.Body.Bytes(), &res)
	if !res.Enriched {
		t.Error("expected enriched true")
	}
	if res.Book.CoverURL == nil || *res.Book.CoverURL == "" {
		t.Error("cover not set")
	}
	if res.Book.Description == nil || *res.Book.Description == "" {
		t.Error("description not set")
	}
}

func TestSeries(t *testing.T) {
	h := newTestServer(t, Config{})

	var res struct {
		Content []struct {
			Name      string `json:"name"`
			BookCount int    `json:"bookCount"`
		} `json:"content"`
	}
	json.Unmarshal(do(t, h, "GET", "/api/v1/series", nil).Body.Bytes(), &res)
	found := false
	for _, s := range res.Content {
		if s.Name == "Война и мир" {
			found = true
			if s.BookCount != 2 {
				t.Errorf("series book count = %d want 2", s.BookCount)
			}
		}
	}
	if !found {
		t.Error("series 'Война и мир' not listed")
	}

	var page struct {
		TotalElements int `json:"totalElements"`
	}
	json.Unmarshal(do(t, h, "GET", "/api/v1/books?series="+url.QueryEscape("Война и мир"), nil).Body.Bytes(), &page)
	if page.TotalElements != 2 {
		t.Errorf("books in series = %d want 2", page.TotalElements)
	}
}

func TestLibrarySync(t *testing.T) {
	h := newTestServer(t, Config{})
	content := []byte("hello epub bytes — unique-xyz")

	// upload -> created + hash
	w := uploadBook(t, h, "a.epub", content)
	if w.Code != http.StatusCreated {
		t.Fatalf("upload = %d want 201", w.Code)
	}
	var m1 map[string]any
	json.Unmarshal(w.Body.Bytes(), &m1)
	id, _ := m1["id"].(string)
	if id == "" || m1["fileHash"] == nil || m1["fileHash"] == "" {
		t.Fatalf("missing id/fileHash: %v", m1)
	}

	// dedup: identical bytes -> 200 + same id (no duplicate)
	w = uploadBook(t, h, "a-copy.epub", content)
	if w.Code != http.StatusOK {
		t.Errorf("dedup upload = %d want 200", w.Code)
	}
	var m2 map[string]any
	json.Unmarshal(w.Body.Bytes(), &m2)
	if m2["id"] != id {
		t.Errorf("dedup id = %v want %v", m2["id"], id)
	}

	// full sync (no since) -> contains the book, no removals
	var d struct {
		ServerTime string           `json:"serverTime"`
		Books      []map[string]any `json:"books"`
		Removed    []string         `json:"removed"`
	}
	json.Unmarshal(do(t, h, "GET", "/api/v1/sync", nil).Body.Bytes(), &d)
	if d.ServerTime == "" || len(d.Books) == 0 {
		t.Fatalf("full sync empty: %+v", d)
	}
	t0 := d.ServerTime

	// mutate -> appears in the delta since t0
	do(t, h, "PATCH", "/api/v1/books/"+id+"/rating", map[string]any{"rating": 4})
	json.Unmarshal(do(t, h, "GET", "/api/v1/sync?since="+url.QueryEscape(t0), nil).Body.Bytes(), &d)
	found := false
	for _, bk := range d.Books {
		if bk["id"] == id {
			found = true
		}
	}
	if !found {
		t.Errorf("mutated book not in delta since %s", t0)
	}

	// soft delete -> tombstone shows up in removed
	if w := do(t, h, "DELETE", "/api/v1/books/"+id, nil); w.Code != http.StatusNoContent {
		t.Errorf("delete = %d want 204", w.Code)
	}
	json.Unmarshal(do(t, h, "GET", "/api/v1/sync?since="+url.QueryEscape(t0), nil).Body.Bytes(), &d)
	gone := false
	for _, rid := range d.Removed {
		if rid == id {
			gone = true
		}
	}
	if !gone {
		t.Errorf("deleted id not in removed: %v", d.Removed)
	}
}
