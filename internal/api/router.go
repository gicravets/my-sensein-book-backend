package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gicravets/my-sensein-book-backend/internal/model"
	"github.com/gicravets/my-sensein-book-backend/internal/store"
)

// Server holds dependencies for the HTTP handlers.
type Server struct {
	st       *store.Store
	bookFile []byte // demo: every book serves this EPUB; real storage comes later
}

// NewRouter wires routes. Shape follows the frontend contract (Komga-style REST
// + CWA data model). bookFile is the EPUB served by GET /books/{id}/file.
func NewRouter(st *store.Store, bookFile []byte) http.Handler {
	s := &Server{st: st, bookFile: bookFile}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.health)

	mux.HandleFunc("GET /api/v1/books", s.listBooks)
	mux.HandleFunc("GET /api/v1/books/{id}", s.getBook)
	mux.HandleFunc("GET /api/v1/books/{id}/file", s.getBookFile)
	mux.HandleFunc("GET /api/v1/shelves", s.listShelves)

	mux.HandleFunc("GET /api/v1/books/{id}/progression", s.getProgression)
	mux.HandleFunc("PUT /api/v1/books/{id}/progression", s.putProgression)
	mux.HandleFunc("PATCH /api/v1/books/{id}/read-progress", s.patchReadProgress)

	mux.HandleFunc("GET /api/v1/highlights", s.listHighlights)
	mux.HandleFunc("POST /api/v1/highlights", s.createHighlight)
	mux.HandleFunc("PATCH /api/v1/highlights/{id}", s.patchHighlight)
	mux.HandleFunc("DELETE /api/v1/highlights/{id}", s.deleteHighlight)

	mux.HandleFunc("GET /api/v1/bookmarks", s.listBookmarks)
	mux.HandleFunc("POST /api/v1/bookmarks", s.createBookmark)
	mux.HandleFunc("DELETE /api/v1/bookmarks/{id}", s.deleteBookmark)

	return cors(logging(mux))
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "my-sensein-book-backend"})
}

func (s *Server) listBooks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("size"))
	res, err := s.st.ListBooks(store.BookQuery{
		Search: q.Get("search"), Shelf: q.Get("shelf"), Sort: q.Get("sort"), Page: page, Size: size,
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
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) getBookFile(w http.ResponseWriter, r *http.Request) {
	if _, ok, _ := s.st.GetBook(r.PathValue("id")); !ok {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", "application/epub+zip")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(s.bookFile)
}

func (s *Server) listShelves(w http.ResponseWriter, _ *http.Request) {
	shelves, err := s.st.ListShelves()
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": shelves, "totalElements": len(shelves)})
}

func (s *Server) getProgression(w http.ResponseWriter, r *http.Request) {
	b, ok, err := s.st.GetBook(r.PathValue("id"))
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, b.ReadProgress)
}

func (s *Server) putProgression(w http.ResponseWriter, r *http.Request) {
	var p model.ReadProgress
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		badRequest(w, err)
		return
	}
	b, ok, err := s.st.PutProgression(r.PathValue("id"), p)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, b.ReadProgress)
}

func (s *Server) patchReadProgress(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Completed bool `json:"completed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		badRequest(w, err)
		return
	}
	b, ok, err := s.st.SetCompleted(r.PathValue("id"), body.Completed)
	if err != nil {
		serverError(w, err)
		return
	}
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, b.ReadProgress)
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
