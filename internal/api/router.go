package api

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// NewRouter wires the HTTP routes. Endpoints mirror the frontend contract;
// handlers are stubs to be backed by SQLite + object storage next.
func NewRouter() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", handleHealth)

	// catalog
	mux.HandleFunc("GET /api/v1/books", handleListBooks)
	mux.HandleFunc("GET /api/v1/books/{id}", handleGetBook)
	mux.HandleFunc("GET /api/v1/books/{id}/file", handleNotImplemented)
	mux.HandleFunc("GET /api/v1/shelves", handleListShelves)

	// reading history
	mux.HandleFunc("GET /api/v1/books/{id}/progression", handleNotImplemented)
	mux.HandleFunc("PUT /api/v1/books/{id}/progression", handleNotImplemented)
	mux.HandleFunc("PATCH /api/v1/books/{id}/read-progress", handleNotImplemented)
	mux.HandleFunc("GET /api/v1/highlights", handleListHighlights)
	mux.HandleFunc("POST /api/v1/highlights", handleNotImplemented)
	mux.HandleFunc("GET /api/v1/bookmarks", handleListBookmarks)
	mux.HandleFunc("POST /api/v1/bookmarks", handleNotImplemented)

	return cors(logging(mux))
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "my-sensein-book-backend"})
}

func handleListBooks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Page[Book]{Content: []Book{}, TotalElements: 0, PageNumber: 0, Size: 50})
}

func handleGetBook(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not Found"})
}

func handleListShelves(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"content": []Shelf{}, "totalElements": 0})
}

func handleListHighlights(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"content": []Highlight{}, "totalElements": 0})
}

func handleListBookmarks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"content": []Bookmark{}, "totalElements": 0})
}

func handleNotImplemented(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "not implemented yet"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
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
