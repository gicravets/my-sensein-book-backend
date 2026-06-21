// Package fb2 converts FictionBook (.fb2) XML into a minimal, valid EPUB so the
// existing epub.js-based readers (web + clients) can render FB2 with all their
// machinery (pagination, themes, highlights, bookmarks) — no separate FB2 reader.
package fb2

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"regexp"
	"strings"
)

// ---- FB2 parsing structures (common subset) ----

type fictionBook struct {
	Description struct {
		TitleInfo struct {
			BookTitle string `xml:"book-title"`
			Authors   []struct {
				First  string `xml:"first-name"`
				Middle string `xml:"middle-name"`
				Last   string `xml:"last-name"`
			} `xml:"author"`
			Lang      string `xml:"lang"`
			Coverpage struct {
				Image struct {
					Href string `xml:"href,attr"`
				} `xml:"image"`
			} `xml:"coverpage"`
		} `xml:"title-info"`
	} `xml:"description"`
	Bodies   []body   `xml:"body"`
	Binaries []binary `xml:"binary"`
}

type body struct {
	Sections []section `xml:"section"`
}

type section struct {
	TitleParas  []string  `xml:"title>p"`
	Paras       []rawXML  `xml:"p"`
	Subtitles   []rawXML  `xml:"subtitle"`
	Poems       []rawXML  `xml:"poem"`
	Subsections []section `xml:"section"`
}

type rawXML struct {
	Inner string `xml:",innerxml"`
}

type binary struct {
	ID          string `xml:"id,attr"`
	ContentType string `xml:"content-type,attr"`
	Data        string `xml:",chardata"`
}

var imageRe = regexp.MustCompile(`<image[^>]*href="#?([^"]+)"[^>]*/?>`)

// IsFB2 reports whether the bytes look like a FictionBook document.
func IsFB2(b []byte) bool {
	head := b
	if len(head) > 600 {
		head = head[:600]
	}
	return bytes.Contains(head, []byte("<FictionBook"))
}

// Meta extracts catalog metadata (title, authors, language, cover image) from FB2.
func Meta(data []byte) (title string, authors []string, lang string, cover []byte) {
	var fb fictionBook
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.CharsetReader = charsetPassthrough
	if err := dec.Decode(&fb); err != nil {
		return "", nil, "", nil
	}
	title = strings.TrimSpace(fb.Description.TitleInfo.BookTitle)
	for _, a := range fb.Description.TitleInfo.Authors {
		name := strings.TrimSpace(strings.Join(strings.Fields(a.First+" "+a.Middle+" "+a.Last), " "))
		if name != "" {
			authors = append(authors, name)
		}
	}
	lang = fb.Description.TitleInfo.Lang
	coverID := strings.TrimPrefix(fb.Description.TitleInfo.Coverpage.Image.Href, "#")
	for _, b := range fb.Binaries {
		if b.ID == coverID && coverID != "" {
			if raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(b.Data), "")); err == nil {
				cover = raw
			}
		}
	}
	return title, authors, lang, cover
}

// ToEPUB converts FB2 bytes to EPUB bytes.
func ToEPUB(data []byte) ([]byte, error) {
	var fb fictionBook
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	dec.CharsetReader = charsetPassthrough
	if err := dec.Decode(&fb); err != nil {
		return nil, fmt.Errorf("parse fb2: %w", err)
	}

	title := strings.TrimSpace(fb.Description.TitleInfo.BookTitle)
	if title == "" {
		title = "Книга"
	}
	var authors []string
	for _, a := range fb.Description.TitleInfo.Authors {
		name := strings.TrimSpace(strings.Join(strings.Fields(a.First+" "+a.Middle+" "+a.Last), " "))
		if name != "" {
			authors = append(authors, name)
		}
	}
	author := strings.Join(authors, ", ")
	lang := fb.Description.TitleInfo.Lang
	if lang == "" {
		lang = "ru"
	}
	coverID := strings.TrimPrefix(fb.Description.TitleInfo.Coverpage.Image.Href, "#")

	// Build chapter XHTML from each top-level <section> across all <body>.
	type chapter struct{ file, title, xhtml string }
	var chapters []chapter
	add := func(sec section, idx int) {
		var sb strings.Builder
		renderSection(&sb, sec)
		ttl := strings.TrimSpace(strings.Join(sec.TitleParas, " "))
		if ttl == "" {
			ttl = fmt.Sprintf("Глава %d", idx)
		}
		chapters = append(chapters, chapter{
			file:  fmt.Sprintf("ch%03d.xhtml", idx),
			title: ttl,
			xhtml: xhtmlDoc(ttl, sb.String(), lang),
		})
	}
	idx := 1
	for _, bd := range fb.Bodies {
		for _, sec := range bd.Sections {
			add(sec, idx)
			idx++
		}
	}
	if len(chapters) == 0 {
		chapters = append(chapters, chapter{file: "ch001.xhtml", title: title,
			xhtml: xhtmlDoc(title, "<p>(пустой документ)</p>", lang)})
	}

	// Decode binaries (images).
	type img struct{ name, mime string; bytes []byte }
	var images []img
	coverFile := ""
	for _, b := range fb.Binaries {
		raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(b.Data), ""))
		if err != nil || b.ID == "" {
			continue
		}
		name := "images/" + b.ID
		images = append(images, img{name: name, mime: b.ContentType, bytes: raw})
		if b.ID == coverID {
			coverFile = name
		}
	}

	// ---- assemble EPUB zip ----
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	// mimetype first, stored (uncompressed)
	mw, _ := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	mw.Write([]byte("application/epub+zip"))

	writeFile := func(name string, content []byte) {
		w, _ := zw.Create(name)
		w.Write(content)
	}
	writeFile("META-INF/container.xml", []byte(containerXML))

	// content.opf
	var manifest, spine, navPoints strings.Builder
	for i, c := range chapters {
		id := fmt.Sprintf("ch%d", i+1)
		manifest.WriteString(fmt.Sprintf(`<item id="%s" href="%s" media-type="application/xhtml+xml"/>`, id, c.file))
		spine.WriteString(fmt.Sprintf(`<itemref idref="%s"/>`, id))
		navPoints.WriteString(fmt.Sprintf(
			`<navPoint id="np%d" playOrder="%d"><navLabel><text>%s</text></navLabel><content src="%s"/></navPoint>`,
			i+1, i+1, html.EscapeString(c.title), c.file))
	}
	for i, im := range images {
		manifest.WriteString(fmt.Sprintf(`<item id="img%d" href="%s" media-type="%s"/>`, i+1, im.name, im.mime))
	}
	coverMeta := ""
	if coverFile != "" {
		coverMeta = `<meta name="cover" content="cover-image"/>`
		manifest.WriteString(fmt.Sprintf(`<item id="cover-image" href="%s" media-type="image/jpeg" properties="cover-image"/>`, coverFile))
	}
	opf := fmt.Sprintf(opfTmpl, html.EscapeString(title), lang, html.EscapeString(author),
		coverMeta, manifest.String(), spine.String())
	writeFile("OEBPS/content.opf", []byte(opf))
	writeFile("OEBPS/toc.ncx", []byte(fmt.Sprintf(ncxTmpl, html.EscapeString(title), navPoints.String())))

	for _, c := range chapters {
		writeFile("OEBPS/"+c.file, []byte(c.xhtml))
	}
	for _, im := range images {
		writeFile("OEBPS/"+im.name, im.bytes)
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func renderSection(sb *strings.Builder, sec section) {
	if t := strings.TrimSpace(strings.Join(sec.TitleParas, " ")); t != "" {
		sb.WriteString("<h2 class=\"fb2-title\">" + html.EscapeString(t) + "</h2>")
	}
	for _, st := range sec.Subtitles {
		sb.WriteString("<h3>" + mapInline(st.Inner) + "</h3>")
	}
	for _, p := range sec.Paras {
		sb.WriteString("<p>" + mapInline(p.Inner) + "</p>")
	}
	for _, pm := range sec.Poems {
		sb.WriteString("<div class=\"poem\">" + mapInline(pm.Inner) + "</div>")
	}
	for _, sub := range sec.Subsections {
		renderSection(sb, sub)
	}
}

func mapInline(s string) string {
	r := strings.NewReplacer(
		"<emphasis>", "<em>", "</emphasis>", "</em>",
		"<strikethrough>", "<s>", "</strikethrough>", "</s>",
		"<empty-line/>", "<br/>", "<empty-line></empty-line>", "<br/>",
		"<v>", "<span class=\"v\">", "</v>", "</span>",
		"<stanza>", "<div class=\"stanza\">", "</stanza>", "</div>",
		"<title>", "", "</title>", "",
	)
	s = r.Replace(s)
	s = imageRe.ReplaceAllString(s, `<img src="images/$1" alt=""/>`)
	return s
}

func xhtmlDoc(title, bodyHTML, lang string) string {
	return fmt.Sprintf(xhtmlTmpl, lang, html.EscapeString(title), bodyHTML)
}

// passthrough for non-UTF8 declared encodings (FB2 is often windows-1251);
// we let the decoder read bytes as-is — adequate for UTF-8 files, which dominate.
func charsetPassthrough(_ string, input io.Reader) (io.Reader, error) {
	return input, nil
}

const containerXML = `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`

const opfTmpl = `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0" unique-identifier="bookid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>%s</dc:title><dc:language>%s</dc:language><dc:creator>%s</dc:creator>%s
  </metadata>
  <manifest><item id="ncx" href="toc.ncx" media-type="application/x-dtbncx+xml"/>%s</manifest>
  <spine toc="ncx">%s</spine>
</package>`

const ncxTmpl = `<?xml version="1.0" encoding="utf-8"?>
<ncx xmlns="http://www.daisy.org/z3986/2005/ncx/" version="2005-1">
  <head/><docTitle><text>%s</text></docTitle><navMap>%s</navMap>
</ncx>`

const xhtmlTmpl = `<?xml version="1.0" encoding="utf-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" lang="%s">
<head><meta charset="utf-8"/><title>%s</title>
<style>img{max-width:100%%;height:auto}.fb2-title{text-align:center;margin:1.4em 0}.poem,.stanza{margin:1em 2em;font-style:italic}.v{display:block}</style>
</head>
<body>%s</body></html>`
