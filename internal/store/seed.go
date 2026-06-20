package store

import (
	"encoding/json"

	"github.com/gicravets/my-sensein-book-backend/internal/model"
)

// seedIfEmpty populates the same demo data as the frontend mock so switching
// NEXT_PUBLIC_API_BASE to this backend shows identical content.
func (s *Store) seedIfEmpty() error {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM books`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	shelves := []model.Shelf{
		{ID: "sh-fav", Name: "Избранное", Kind: "normal", IsPublic: false},
		{ID: "sh-classics", Name: "Классика", Kind: "normal", IsPublic: true},
		{ID: "sh-now", Name: "Читаю сейчас", Kind: "smart", IsPublic: false},
		{ID: "sh-toread", Name: "В планах", Kind: "normal", IsPublic: false},
	}
	for _, sh := range shelves {
		raw, _ := json.Marshal(sh)
		if _, err := s.db.Exec(`INSERT INTO shelves(id,data) VALUES(?,?)`, sh.ID, string(raw)); err != nil {
			return err
		}
	}

	rp := func(p, tp float64, page, total int, done bool, at, dev string) *model.ReadProgress {
		return &model.ReadProgress{Progression: p, TotalProgression: tp, Page: page, TotalPages: total,
			Completed: done, LastReadAt: sp(at), DeviceName: sp(dev)}
	}
	books := []model.Book{
		{ID: "bk-1", Title: "Pride and Prejudice", Authors: []string{"Jane Austen"}, Format: "EPUB",
			Size: 1200000, Language: sp("en"), Publisher: sp("Project Gutenberg"),
			Description: sp("Один из самых известных романов Джейн Остин: ирония нравов, гордость и предубеждения."),
			Tags: []string{"classic", "romance"}, AddedAt: "2026-06-10T10:00:00Z", CoverSeed: "Pride and Prejudice",
			ShelfIDs: []string{"sh-classics", "sh-now"},
			ReadProgress: rp(0.4, 0.07, 24, 357, false, "2026-06-19T21:10:00Z", "My.Sensein.Book iPhone")},
		{ID: "bk-2", Title: "Дубровский", Authors: []string{"Александр Сергеевич Пушкин"}, Format: "FB2",
			Size: 1200000, Language: sp("ru"), Tags: []string{"classic"}, AddedAt: "2026-06-10T10:00:00Z",
			Description: sp("Незаконченный роман А. С. Пушкина о благородном разбойнике."),
			CoverSeed:   "Дубровский", ShelfIDs: []string{"sh-classics", "sh-fav"},
			ReadProgress: rp(0.68, 0.84, 96, 114, false, "2026-06-19T20:30:00Z", "My.Sensein.Book iPhone")},
		{ID: "bk-3", Title: "Война и мир. Том 1", Authors: []string{"Лев Толстой"}, Format: "EPUB",
			Size: 1200000, Language: sp("ru"), Series: sp("Война и мир"), SeriesIndex: fp(1),
			Tags: []string{"classic", "epic"}, AddedAt: "2026-06-10T10:00:00Z", CoverSeed: "Война и мир. Том 1",
			ShelfIDs: []string{"sh-classics", "sh-toread"}},
		{ID: "bk-4", Title: "Метро 2033", Authors: []string{"Дмитрий Глуховский"}, Format: "EPUB",
			Size: 1200000, Language: sp("ru"), Tags: []string{"sci-fi"}, AddedAt: "2026-06-10T10:00:00Z",
			CoverSeed: "Метро 2033", ShelfIDs: []string{"sh-toread"},
			ReadProgress: rp(1, 1, 540, 540, true, "2026-05-30T18:00:00Z", "iPad")},
		{ID: "bk-5", Title: "The Hobbit", Authors: []string{"J.R.R. Tolkien"}, Format: "EPUB",
			Size: 1200000, Language: sp("en"), Tags: []string{"fantasy"}, AddedAt: "2026-06-10T10:00:00Z",
			CoverSeed: "The Hobbit", ShelfIDs: []string{"sh-fav"}},
		{ID: "bk-6", Title: "Преступление и наказание", Authors: []string{"Фёдор Достоевский"}, Format: "EPUB",
			Size: 1200000, Language: sp("ru"), Tags: []string{"classic"}, AddedAt: "2026-06-10T10:00:00Z",
			CoverSeed: "Преступление и наказание", ShelfIDs: []string{"sh-classics"},
			ReadProgress: rp(0.12, 0.22, 130, 600, false, "2026-06-15T09:00:00Z", "My.Sensein.Book iPhone")},
	}
	for i := range books {
		if books[i].Authors == nil {
			books[i].Authors = []string{}
		}
		if books[i].Tags == nil {
			books[i].Tags = []string{}
		}
		if books[i].ShelfIDs == nil {
			books[i].ShelfIDs = []string{}
		}
		if err := s.SaveBook(books[i]); err != nil {
			return err
		}
	}

	hl := []model.Highlight{
		{ID: "hl-1", BookID: "bk-1", Color: "yellow", CreatedAt: "2026-06-18T12:05:00Z",
			Text: "It is a truth universally acknowledged, that a single man in possession of a good fortune, must be in want of a wife.",
			Note: sp("Знаменитая первая строка — задаёт иронический тон."),
			Locator: model.Locator{Href: sp("OEBPS/Text/main1.xml"), Type: sp("xhtml"), Value: sp("p1"), Progression: 0.01}},
		{ID: "hl-2", BookID: "bk-2", Color: "green", CreatedAt: "2026-06-17T18:30:00Z",
			Text: "Кирила Петрович был с ним необыкновенно ласков.",
			Locator: model.Locator{Href: sp("ch2"), Type: sp("fb2"), Value: sp("p40"), Progression: 0.2}},
		{ID: "hl-3", BookID: "bk-6", Color: "pink", CreatedAt: "2026-06-15T09:30:00Z",
			Text: "Тварь ли я дрожащая или право имею.", Note: sp("Ключевой вопрос Раскольникова."),
			Locator: model.Locator{Href: sp("part3"), Type: sp("xhtml"), Value: sp("p210"), Progression: 0.5}},
	}
	for _, h := range hl {
		raw, _ := json.Marshal(h)
		if _, err := s.db.Exec(`INSERT INTO highlights(id,book_id,data) VALUES(?,?,?)`, h.ID, h.BookID, string(raw)); err != nil {
			return err
		}
	}

	bm := []model.Bookmark{
		{ID: "bm-1", BookID: "bk-1", Label: "Глава 3 — бал в Незерфилде", CreatedAt: "2026-06-18T12:00:00Z",
			Locator: model.Locator{Href: sp("OEBPS/Text/main3.xml"), Type: sp("xhtml"), Value: sp("ch3"), Progression: 0.4}},
		{ID: "bm-2", BookID: "bk-2", Label: "Пожар в усадьбе", CreatedAt: "2026-06-17T19:00:00Z",
			Locator: model.Locator{Href: sp("ch5"), Type: sp("fb2"), Value: sp("p120"), Progression: 0.68}},
	}
	for _, b := range bm {
		raw, _ := json.Marshal(b)
		if _, err := s.db.Exec(`INSERT INTO bookmarks(id,book_id,data) VALUES(?,?,?)`, b.ID, b.BookID, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

func sp(s string) *string  { return &s }
func fp(f float64) *float64 { return &f }
