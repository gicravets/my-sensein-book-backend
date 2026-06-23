# my-sensein-book-backend

Бэкенд проекта **My.Sensein.Book** — тонкий сервис на **Go** для хранения библиотеки и истории чтения (позиции, полки, закладки, выделения с заметками) и доступа к ним по REST. Разработчик: **Kravitz Geroge**.

## Идея

Не порт Calibre-Web-Automated и не клон Komga, а синтез:
- **форма API** — в стиле Komga (чистый REST + Readium progression + API-ключи);
- **модель данных** — в стиле CWA (полки, закладки, выделения с `text`/`color`/`note`).

Парсинг книг выполняется на клиентах (iOS/веб), поэтому серверу не нужен движок Calibre — он остаётся тонким (файлы + метаданные + история), легко контейнеризуется и нативно дружелюбен к доступу ИИ по MCP (через SQLite/Postgres).

Контракт API повторяет типы фронтенда:
[`my-sensein-book-frontend/src/lib/types.ts`](https://github.com/gicravets/my-sensein-book-frontend/blob/main/src/lib/types.ts).
Go-структуры — в [`internal/api/models.go`](internal/api/models.go).

## Статус

Рабочий MVP на **SQLite** (pure-Go `modernc.org/sqlite`, статический бинарь, образ ~10–13 МБ):
- весь контракт реализован — каталог, полки (со счётчиками), поиск/сортировка/фильтр, прогресс чтения (auto-complete у конца), отметка «прочитано», CRUD выделений и закладок, отдача файла книги (встроенный EPUB через `embed`);
- сид теми же данными, что в моке фронта → переключение `NEXT_PUBLIC_API_BASE` показывает идентичный контент;
- данные переживают рестарт; для персистентности вне контейнера смонтируйте том на `DB_PATH` (`-e DB_PATH=/data/app.sqlite -v msb-data:/data`);
- проверено end-to-end в браузере: фронт + читалка epub.js работают поверх этого бэкенда.

Дальше: auth/API-ключи (заголовок `X-API-Key` уже разрешён в CORS), реальное файловое хранилище вместо встроенного образца, доступ ИИ по MCP к SQLite.

## Запуск (только Docker)

```bash
docker build -t my-sensein-book-backend .
docker run --rm -p 8080:8080 my-sensein-book-backend
curl localhost:8080/health
```

Или dev без образа:

```bash
docker run --rm -v "$PWD":/src -w /src -p 8080:8080 golang:1.25-alpine go run .
```

Режимы: `DEMO_MODE=true` — read-only демо (записи → 403, отдаёт сид-данные); `APP_VERSION=vX` — версия для `/version` и сравнения в `/update`; первичная настройка — `POST /setup/claim` (создаёт admin-ключ в БД, если не задан `API_KEY`).

**Файловое хранилище** (`STORAGE_DRIVER`):
- `local` (по умолчанию) — `FILES_DIR`;
- `s3` — S3-совместимое (AWS S3 / MinIO / R2): `S3_ENDPOINT S3_BUCKET S3_REGION S3_KEY S3_SECRET S3_SSL`;
- `webdav` — `WEBDAV_URL WEBDAV_USER WEBDAV_PASS`.

## Эндпоинты (контракт)

```
GET   /health
GET   /api/v1/setup            POST /api/v1/setup/claim     (first-run wizard: claim admin key)
GET   /api/v1/version          GET  /api/v1/update          (build version + GitHub update check, cached ~1h)
GET   /api/v1/books            GET /api/v1/books/{id}      GET /api/v1/books/{id}/file
GET   /api/v1/search           (full-text over book metadata + saved quotes; FTS5, ё/е-folded)
GET   /api/v1/series           (multi-volume groupings; books via GET /api/v1/books?series=Name)
POST  /api/v1/books/{id}/enrich (fetch cover + description from Open Library; fills missing fields)
POST  /api/v1/library/scan     (import new files from WATCH_DIR now; also auto-scanned every 30s)
DELETE /api/v1/books/{id}       (soft delete + tombstone)
GET   /api/v1/sync?since=       (library delta: changed books + removed ids + serverTime — sync-point)
GET   /api/v1/users           POST /api/v1/users           (family users; admin creates them)
POST  /api/v1/auth/pair?userId=   (pair a device to a specific user; device key resolves to that user)
GET   /api/v1/shelves          POST /api/v1/shelves   PATCH /api/v1/shelves/{id} {isPublic}   (per-user + public/shared shelves)
GET   /api/v1/readlists       POST /api/v1/readlists   DELETE /api/v1/readlists/{id}   GET /api/v1/readlists/{id}/books   POST|DELETE /api/v1/readlists/{id}/books/{bookId}   (ordered reading lists)
GET   /api/v1/smart-shelves     POST /api/v1/smart-shelves   DELETE /api/v1/smart-shelves/{id}   GET /api/v1/smart-shelves/{id}/books   (dynamic rule-based shelves)
GET   /api/v1/preferences       PUT  /api/v1/preferences     (per-user reader settings sync)
GET   /api/v1/books/{id}/progression     PUT /api/v1/books/{id}/progression   (per-user)
PATCH /api/v1/books/{id}/read-progress                                       (per-user)
GET   /api/v1/highlights       POST /api/v1/highlights
GET   /api/v1/bookmarks        POST /api/v1/bookmarks
```

## Документы (вне этого репо)

> Этот репозиторий — **только код**. Контракты, ресёрч по хранилищу, журналы и заметки для
> агентов живут в `my-sensein-book-docs` (общий PWA/серверный раздел).
> **Начни с** [`../my-sensein-book-docs/REGULATIONS.md`](../my-sensein-book-docs/).

- Доки: [`../my-sensein-book-docs`](../my-sensein-book-docs/) ([GitHub](https://github.com/gicravets/my-sensein-book-docs)) — см. `AGENTS-backend.md`, `backend-storage-research.md`
- Деплой всего стека: `docker compose up --build -d` (см. `docker-compose.yml` в этом репо)

## Связанные репозитории

- Веб-PWA: [my-sensein-book-frontend](https://github.com/gicravets/my-sensein-book-frontend)
- iOS: [my-sensein-book-ios](https://github.com/gicravets/my-sensein-book-ios)
