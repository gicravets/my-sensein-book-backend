package api

// These structs mirror the frontend API contract
// (my-sensein-book-frontend/src/lib/types.ts). API shape follows Komga's REST
// conventions; the data model borrows CWA's richer reading-history entities.

type BookFormat string

const (
	FormatEPUB BookFormat = "EPUB"
	FormatFB2  BookFormat = "FB2"
	FormatPDF  BookFormat = "PDF"
)

type ReadProgress struct {
	Progression      float64 `json:"progression"`
	TotalProgression float64 `json:"totalProgression"`
	Page             int     `json:"page"`
	TotalPages       int     `json:"totalPages"`
	Completed        bool    `json:"completed"`
	LastReadAt       *string `json:"lastReadAt"`
	DeviceName       *string `json:"deviceName"`
}

type Book struct {
	ID            string        `json:"id"`
	Title         string        `json:"title"`
	Authors       []string      `json:"authors"`
	Series        *string       `json:"series"`
	SeriesIndex   *float64      `json:"seriesIndex"`
	Format        BookFormat    `json:"format"`
	Size          int64         `json:"size"`
	Language      *string       `json:"language"`
	Publisher     *string       `json:"publisher"`
	ISBN          *string       `json:"isbn"`
	Description   *string       `json:"description"`
	Tags          []string      `json:"tags"`
	AddedAt       string        `json:"addedAt"`
	CoverSeed     string        `json:"coverSeed"`
	CoverURL      *string       `json:"coverUrl"`
	ReadProgress  *ReadProgress `json:"readProgress"`
	ShelfIDs      []string      `json:"shelfIds"`
}

type Shelf struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"` // "normal" | "smart"
	BookCount int    `json:"bookCount"`
	IsPublic  bool   `json:"isPublic"`
}

type Locator struct {
	Href        *string `json:"href"`
	Type        *string `json:"type"`
	Value       *string `json:"value"`
	Progression float64 `json:"progression"`
}

type Bookmark struct {
	ID        string  `json:"id"`
	BookID    string  `json:"bookId"`
	Locator   Locator `json:"locator"`
	Label     string  `json:"label"`
	CreatedAt string  `json:"createdAt"`
}

type Highlight struct {
	ID        string  `json:"id"`
	BookID    string  `json:"bookId"`
	Text      string  `json:"text"`
	Color     string  `json:"color"` // yellow|green|blue|pink|orange
	Note      *string `json:"note"`
	Locator   Locator `json:"locator"`
	CreatedAt string  `json:"createdAt"`
}

// Page is the paginated envelope returned by list endpoints (Komga style).
type Page[T any] struct {
	Content       []T `json:"content"`
	TotalElements int `json:"totalElements"`
	PageNumber    int `json:"page"`
	Size          int `json:"size"`
}
