package main

import (
	"context"
	_ "embed"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gicravets/my-sensein-book-backend/internal/api"
	"github.com/gicravets/my-sensein-book-backend/internal/store"
)

//go:embed assets/sample.epub
var sampleEPUB []byte

func main() {
	dbPath := env("DB_PATH", "app.sqlite")
	filesDir := env("FILES_DIR", filepath.Join(filepath.Dir(dbPath), "files"))
	st, err := store.Open(dbPath, filesDir)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	requireAuth := env("REQUIRE_AUTH", "false") == "true"
	masterKey := env("API_KEY", "")
	addr := ":" + env("PORT", "8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewRouter(st, sampleEPUB, requireAuth, masterKey),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("my-sensein-book-backend listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
	log.Println("stopped")
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
