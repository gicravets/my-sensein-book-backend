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

Скелет: HTTP-роутер с маршрутами контракта. Реализовано: `GET /health` и заглушки списков. Дальше — SQLite-хранилище, auth/API-ключи, файловое хранилище, запись прогресса/выделений/закладок.

## Запуск (только Docker)

```bash
docker build -t my-sensein-book-backend .
docker run --rm -p 8080:8080 my-sensein-book-backend
curl localhost:8080/health
```

Или dev без образа:

```bash
docker run --rm -v "$PWD":/src -w /src -p 8080:8080 golang:1.23-alpine go run .
```

## Эндпоинты (контракт)

```
GET   /health
GET   /api/v1/books            GET /api/v1/books/{id}      GET /api/v1/books/{id}/file
GET   /api/v1/shelves
GET   /api/v1/books/{id}/progression     PUT /api/v1/books/{id}/progression
PATCH /api/v1/books/{id}/read-progress
GET   /api/v1/highlights       POST /api/v1/highlights
GET   /api/v1/bookmarks        POST /api/v1/bookmarks
```

## Связанные репозитории

- Веб-PWA: [my-sensein-book-frontend](https://github.com/gicravets/my-sensein-book-frontend)
- iOS: [my-sensein-book-ios](https://github.com/gicravets/my-sensein-book-ios)
