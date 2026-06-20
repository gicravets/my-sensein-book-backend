// Package epub does minimal EPUB metadata + cover extraction (no deps).
// An EPUB is a zip: META-INF/container.xml -> OPF (package) with dc:* metadata
// and a manifest; the cover is the manifest item referenced by <meta name="cover">
// or marked properties="cover-image".
package epub

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"path"
	"strings"
)

type Meta struct {
	Title       string
	Authors     []string
	Language    string
	Description string
	Cover       []byte
	CoverType   string
}

type container struct {
	Rootfiles []struct {
		FullPath string `xml:"full-path,attr"`
	} `xml:"rootfiles>rootfile"`
}

type pkg struct {
	Metadata struct {
		Title       []string `xml:"title"`
		Creator     []string `xml:"creator"`
		Language    []string `xml:"language"`
		Description []string `xml:"description"`
		Meta        []struct {
			Name     string `xml:"name,attr"`
			Content  string `xml:"content,attr"`
			Property string `xml:"property,attr"`
		} `xml:"meta"`
	} `xml:"metadata"`
	Manifest struct {
		Items []struct {
			ID         string `xml:"id,attr"`
			Href       string `xml:"href,attr"`
			MediaType  string `xml:"media-type,attr"`
			Properties string `xml:"properties,attr"`
		} `xml:"item"`
	} `xml:"manifest"`
}

// Parse extracts metadata + cover from raw EPUB bytes. Best-effort: returns
// whatever it can; missing fields stay empty.
func Parse(data []byte) (Meta, error) {
	var m Meta
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return m, err
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}

	opfPath := "OEBPS/content.opf"
	if c := files["META-INF/container.xml"]; c != nil {
		var con container
		if b, err := readZip(c); err == nil {
			if xml.Unmarshal(b, &con) == nil && len(con.Rootfiles) > 0 {
				opfPath = con.Rootfiles[0].FullPath
			}
		}
	}

	opfFile := files[opfPath]
	if opfFile == nil {
		return m, nil
	}
	opfBytes, err := readZip(opfFile)
	if err != nil {
		return m, err
	}
	var p pkg
	if err := xml.Unmarshal(opfBytes, &p); err != nil {
		return m, err
	}

	if len(p.Metadata.Title) > 0 {
		m.Title = strings.TrimSpace(p.Metadata.Title[0])
	}
	for _, c := range p.Metadata.Creator {
		if s := strings.TrimSpace(c); s != "" {
			m.Authors = append(m.Authors, s)
		}
	}
	if len(p.Metadata.Language) > 0 {
		m.Language = strings.TrimSpace(p.Metadata.Language[0])
	}
	if len(p.Metadata.Description) > 0 {
		m.Description = stripTags(p.Metadata.Description[0])
	}

	// resolve cover: meta name=cover -> item id; or properties="cover-image"
	coverID := ""
	for _, mt := range p.Metadata.Meta {
		if mt.Name == "cover" {
			coverID = mt.Content
		}
	}
	opfDir := path.Dir(opfPath)
	var coverHref, coverType string
	for _, it := range p.Manifest.Items {
		if (coverID != "" && it.ID == coverID) || strings.Contains(it.Properties, "cover-image") {
			coverHref, coverType = it.Href, it.MediaType
			break
		}
	}
	if coverHref != "" {
		full := path.Join(opfDir, coverHref)
		if cf := files[full]; cf != nil {
			if b, err := readZip(cf); err == nil {
				m.Cover = b
				m.CoverType = coverType
			}
		}
	}
	return m, nil
}

func readZip(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func stripTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>':
			if depth > 0 {
				depth--
			}
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
