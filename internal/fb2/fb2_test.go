package fb2

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
)

const sampleFB2 = `<?xml version="1.0" encoding="utf-8"?>
<FictionBook xmlns="http://www.gribuser.ru/xml/fictionbook/2.0" xmlns:l="http://www.w3.org/1999/xlink">
  <description><title-info>
    <author><first-name>Александр</first-name><last-name>Пушкин</last-name></author>
    <book-title>Дубровский</book-title><lang>ru</lang>
    <coverpage><image l:href="#cover.png"/></coverpage>
  </title-info></description>
  <body>
    <section><title><p>Глава I</p></title>
      <p>Несколько лет тому назад в одном из своих поместий жил <emphasis>старинный</emphasis> русский барин.</p>
      <p>Богатство, знатный род и связи давали ему большой вес.</p>
    </section>
    <section><title><p>Глава II</p></title>
      <p>Второй раздел книги с <strong>важным</strong> текстом.</p>
    </section>
  </body>
  <binary id="cover.png" content-type="image/png">iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==</binary>
</FictionBook>`

func TestToEPUB(t *testing.T) {
	if !IsFB2([]byte(sampleFB2)) {
		t.Fatal("IsFB2 should detect the sample")
	}
	epub, err := ToEPUB([]byte(sampleFB2))
	if err != nil {
		t.Fatalf("ToEPUB: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(epub), int64(len(epub)))
	if err != nil {
		t.Fatalf("result is not a valid zip: %v", err)
	}
	want := map[string]bool{"mimetype": false, "META-INF/container.xml": false,
		"OEBPS/content.opf": false, "OEBPS/toc.ncx": false,
		"OEBPS/ch001.xhtml": false, "OEBPS/ch002.xhtml": false, "OEBPS/images/cover.png": false}
	var ch1 string
	for _, f := range zr.File {
		if _, ok := want[f.Name]; ok {
			want[f.Name] = true
		}
		if f.Name == "mimetype" && f.Method != zip.Store {
			t.Error("mimetype must be stored (uncompressed)")
		}
		if f.Name == "OEBPS/ch001.xhtml" {
			rc, _ := f.Open()
			var b bytes.Buffer
			b.ReadFrom(rc)
			rc.Close()
			ch1 = b.String()
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing EPUB entry: %s", name)
		}
	}
	if !strings.Contains(ch1, "Глава I") || !strings.Contains(ch1, "<em>старинный</em>") {
		t.Errorf("ch001 missing expected content; got:\n%s", ch1)
	}
}
