# AGENTS.md — my-sensein-book-backend (handoff for AI agents)

Thin **Go** backend for **My.Sensein.Book** (developer: Kravitz Geroge). Stores the
library + reading history and serves a REST API consumed by the web PWA and iOS app.

## Design (read this first)
- **Not** a port of Calibre-Web and **not** a Komga clone. Synthesis: **API shape
  inspired by Komga** (take only what we need), **data model inspired by CWA**
  (shelves, bookmarks, highlights with text/color/note). Books are parsed on the
  clients, so the server needs **no Calibre engine** — it stays thin.
- Storage: **SQLite via `modernc.org/sqlite` (pure Go)** → static `CGO_ENABLED=0`
  build, ~10–13MB distroless image. Entities stored as **JSON blobs** per row;
  filtering/sorting done in Go (fine for personal-scale libraries).

## ⚠️ Hard rules / gotchas
- **Build & run ONLY in Docker** (project-wide rule). Never install Go on the host.
- **`modernc.org/sqlite` v1.52 needs Go ≥ 1.25** → build with `golang:1.25-alpine`
  and `go 1.25` in go.mod. golang:1.23 fails with a toolchain error.
- macOS has no `timeout` CLI — don't rely on it in helper scripts.

## Run (Docker only)
```bash
docker build -t my-sensein-book-backend .
docker run -d --name msb-be -p 8090:8080 \
  -e DB_PATH=/data/app.sqlite -v msb-data:/data my-sensein-book-backend
curl localhost:8090/health
# dev (no image): docker run --rm -v "$PWD":/src -w /src -p 8090:8080 golang:1.25-alpine go run .
# vet+build check:  ... golang:1.25-alpine sh -c "go vet ./... && CGO_ENABLED=0 go build -o /tmp/s ."
```
Env: `PORT` (8080), `DB_PATH` (app.sqlite), `FILES_DIR` (dir(DB)/files for uploaded
book files+covers), `REQUIRE_AUTH` (false), `API_KEY` (master key when auth on).

## Layout
- `main.go` — server bootstrap, embeds `assets/sample.epub`.
- `internal/model` — domain types = the API contract (mirror of frontend `types.ts`).
- `internal/store` — SQLite store: books/shelves/highlights/bookmarks/devices/pairings,
  filters (search/shelf/tag/author/series/language/publisher/format/status), sorts,
  ratings, archived, file/cover storage, device keys + QR pairing.
- `internal/api` — `NewRouter`, handlers, CORS, optional auth gate.
- `internal/epub` — dependency-free EPUB metadata + cover extraction (for upload).

## API (selected)
- `GET /health`
- `GET /api/v1/books?search,shelf,tag,author,series,language,publisher,format,filter,sort,page,size`
  filter ∈ read|unread|archived|rated|downloaded|hot (default hides archived);
  sort ∈ recent|recent_old|title|title_desc|author|author_desc|pub|pub_desc|rating|random
- `POST /api/v1/books` (multipart upload), `GET /api/v1/books/{id}` `/file` `/cover`
- `PUT /api/v1/books/{id}/progression`, `PATCH .../read-progress|rating|archived`
- `GET/POST /api/v1/highlights`, `PATCH/DELETE /highlights/{id}`; bookmarks same
- `GET/POST /api/v1/shelves`, `DELETE /shelves/{id}`, `POST|DELETE /shelves/{id}/books/{bookId}`
- **Auth/pairing** (device-token, ref Calibre-Web remote-login):
  `POST /auth/device`, `POST /auth/pair` (token+QR payload {url,t}),
  `POST /auth/pair/claim` (single-use → device key), `GET /auth/pair/status`.
  Clients send `X-API-Key`. `/health` and `/auth/*` bypass the gate.

## State
Library/browse/upload/shelves/ratings/archive + QR pairing + device-token auth: done,
curl-verified. Next: richer sync (annotation pull, precise-position locator bridge).

## Related
- Frontend (web PWA): github.com/gicravets/my-sensein-book-frontend
- iOS app: github.com/gicravets/my-sensein-book-ios
- Design/QA notes (outside repos): `~/Documents/doc-vpt/journals/{vpt,qa}-journal.json`
