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
