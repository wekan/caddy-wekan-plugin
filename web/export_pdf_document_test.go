package web

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"testing"
)

// The PDF of every card document and of pdf-extra.json, as Node.js WeKan's
// pdfDocument.js writes it (generate-pdf.mjs): documentToLines's lines and
// buildPdfBuffer's bytes, every one; encodeWinAnsi, flattenMarkdown and
// wrapTextBlock on their edges; and a PNG's pixels as the PDF has them.
func TestPdfDocumentIsWeKans(t *testing.T) {
	raw, err := os.ReadFile("testdata/export/pdf.golden")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Documents map[string]struct {
			Lines json.RawMessage `json:"lines"`
			PDF   string          `json:"pdf"`
		} `json:"documents"`
		Encode  map[string]string          `json:"encode"`
		Flatten map[string]string          `json:"flatten"`
		Wrap    map[string][]string        `json:"wrap"`
		Inline  map[string][]pdfRun        `json:"inline"`
		Rich    map[string]json.RawMessage `json:"rich"`
		PNG     struct {
			File          string `json:"file"`
			Width, Height int
			RGB           string `json:"rgb"`
		} `json:"png"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	docs := map[string][]cdBlock{}
	for _, f := range []string{"testdata/export/card-documents.golden", "testdata/export/pdf-extra.json"} {
		data, _ := os.ReadFile(f)
		var m map[string][]cdBlock
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(f, err)
		}
		for k, v := range m {
			docs[k] = v
		}
	}
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xC0, 0x00, 0x11, 0x08, 0x00, 0x10, 0x00, 0x20, 0x03, 0x01, 0x22, 0x00, 0x02,
		0x11, 0x01, 0x03, 0x11, 0x01, 0xFF, 0xD9, 0x00, 0x00, 0x00, 0x00, 0x00}
	if len(golden.Documents) != len(docs) || len(docs) < 5 {
		t.Fatalf("the documents: %d %d", len(golden.Documents), len(docs))
	}
	for name, doc := range docs {
		for i := range doc {
			for k := range doc[i].Images {
				doc[i].Images[k].Data = jpeg
			}
		}
		want := golden.Documents[name]
		lines := documentToLines(doc, []string{"#", "Name", "Size", "Type", "Uploaded at", "Uploaded by"})
		g, _ := json.Marshal(lines)
		var a, b any
		_ = json.Unmarshal(g, &a)
		_ = json.Unmarshal(want.Lines, &b)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s lines:\n%s\nnot WeKan's\n%s", name, g, want.Lines)
			continue
		}
		wp, _ := base64.StdEncoding.DecodeString(want.PDF)
		if got := buildPdfBuffer(lines); !bytes.Equal(got, wp) {
			i := 0
			for i < len(got) && i < len(wp) && got[i] == wp[i] {
				i++
			}
			t.Errorf("%s: the PDF differs at byte %d:\n%q\nnot WeKan's\n%q", name, i, got[max(0, i-60):min(len(got), i+60)], wp[max(0, i-60):min(len(wp), i+60)])
		}
	}
	for s, want := range golden.Encode {
		if got := base64.StdEncoding.EncodeToString(encodeWinAnsi(s)); got != want {
			w, _ := base64.StdEncoding.DecodeString(want)
			t.Errorf("encodeWinAnsi(%q) = %q, not %q", s, encodeWinAnsi(s), w)
		}
	}
	for s, want := range golden.Flatten {
		if got := flattenMarkdown(s); got != want {
			t.Errorf("flattenMarkdown(%q) = %q, not %q", s, got, want)
		}
	}
	for k, want := range golden.Wrap {
		i := bytes.IndexByte([]byte(k), '|')
		if got := wrapTextBlock(k[i+1:], k[:i]); !reflect.DeepEqual(got, want) && !(len(got) == 0 && len(want) == 0) {
			t.Errorf("wrapTextBlock(%q, %q) = %q, not %q", k[i+1:], k[:i], got, want)
		}
	}
	if len(golden.Inline) == 0 || len(golden.Rich) == 0 {
		t.Fatal("the inline and rich cases")
	}
	for s, want := range golden.Inline {
		if got := inlineRuns(s); !reflect.DeepEqual(got, want) {
			t.Errorf("inlineRuns(%q) = %v, not %v", s, got, want)
		}
	}
	for k, want := range golden.Rich {
		i := bytes.IndexByte([]byte(k), '|')
		g, _ := json.Marshal(wrapRichTextBlock(k[i+1:], k[:i]))
		var a, b any
		_ = json.Unmarshal(g, &a)
		_ = json.Unmarshal(want, &b)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("wrapRichTextBlock(%q, %q):\n%s\nnot\n%s", k[i+1:], k[:i], g, want)
		}
	}
	file, _ := base64.StdEncoding.DecodeString(golden.PNG.File)
	img := pngImage(file)
	if img == nil || img.width != golden.PNG.Width || img.height != golden.PNG.Height {
		t.Fatalf("the PNG: %+v", img)
	}
	zr, _ := zlib.NewReader(bytes.NewReader(img.data))
	rgb, _ := io.ReadAll(zr)
	if base64.StdEncoding.EncodeToString(rgb) != golden.PNG.RGB {
		t.Errorf("the PNG's pixels: %v", rgb)
	}
	// Negative: what is no picture is none.
	if pngImage([]byte("not a png")) != nil || preparePdfImage(cdImage{Ext: "gif", Data: jpeg}) != nil {
		t.Error("negative: not a picture the PDF draws")
	}
}
