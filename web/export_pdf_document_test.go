package web

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
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

func TestExportPDFUsesUnicodeFont(t *testing.T) {
	baseline := []pdfLine{pdfPlain("Latin only", false)}
	if !bytes.Equal(buildExportPdfBuffer(baseline), buildPdfBuffer(baseline)) {
		t.Fatal("WinAnsi-only exports changed")
	}
	output := buildExportPdfBuffer([]pdfLine{pdfPlain("日本語", false)})
	for _, fragment := range []string{"/FontFile2", "/ToUnicode", "/FU ", "<0001> <65E5>", "<0002> <672C>", "<0003> <8A9E>", "<000100020003> Tj"} {
		if !bytes.Contains(output, []byte(fragment)) {
			t.Errorf("Unicode PDF does not contain %q", fragment)
		}
	}
	mixed := buildExportPdfBuffer([]pdfLine{pdfPlain("日😀本", false)})
	for _, fragment := range []string{"+UnifontUpper", "/FS ", "<0001> <D83DDE00>", "/FS 10 Tf\n<0001> Tj\n/FU 10 Tf\n<0002> Tj"} {
		if !bytes.Contains(mixed, []byte(fragment)) {
			t.Errorf("mixed-plane PDF does not contain %q", fragment)
		}
	}
	upper, err := loadPDFUnifontUpper()
	if err != nil {
		t.Fatal(err)
	}
	if glyph, err := upper.face.GlyphIndex(nil, '😀'); err != nil || glyph == 0 {
		t.Fatal("bundled Unifont Upper is missing the supplementary glyph")
	}

	program, err := loadPDFUnifont()
	if err != nil {
		t.Fatal(err)
	}
	glyph, err := program.face.GlyphIndex(nil, '日')
	if err != nil || glyph == 0 {
		t.Fatal("bundled Unifont is missing the Japanese glyph")
	}
}

func TestPDFWrapKeepsSupplementaryCharacter(t *testing.T) {
	before := strings.Repeat("a", pdfTextWidth-1)
	text := before + "😀z"
	if got := wrapLine(text, pdfTextWidth); !reflect.DeepEqual(got, []string{before, "😀z"}) {
		t.Fatalf("wrapped plain text: %q", got)
	}
	rich := wrapRichTextBlock("**"+text+"**", "")
	if len(rich) != 2 || !strings.Contains(string(buildExportPdfBuffer(rich)), "<D83DDE00>") {
		t.Fatalf("wrapped rich text: %+v", rich)
	}
	if got := pdfLineChunks("😀z", 1); !reflect.DeepEqual(got, []string{"😀", "z"}) {
		t.Fatalf("one-unit width: %q", got)
	}
}

func TestExportPDFSubsetsOutlines(t *testing.T) {
	for _, tc := range []struct {
		load          func() (pdfFontProgram, error)
		keep, discard rune
	}{
		{loadPDFUnifont, '日', '本'},
		{loadPDFUnifontUpper, '😀', '😁'},
	} {
		program, err := tc.load()
		if err != nil {
			t.Fatal(err)
		}
		keep, _ := program.face.GlyphIndex(nil, tc.keep)
		discard, _ := program.face.GlyphIndex(nil, tc.discard)
		if keep == 0 || discard == 0 {
			t.Fatal("test font lacks glyphs")
		}
		subset := subsetPDFTrueType(program.data, []sfnt.GlyphIndex{keep}, program.face.NumGlyphs())
		if pdfFontChecksum(subset) != 0xB1B0AFBA || len(subset) >= len(program.data)/4 {
			t.Fatalf("invalid subset size or checksum: %d bytes", len(subset))
		}
		font, err := sfnt.Parse(subset)
		if err != nil {
			t.Fatal(err)
		}
		got, err := font.LoadGlyph(nil, keep, fixed.I(16), nil)
		want, originalErr := program.face.LoadGlyph(nil, keep, fixed.I(16), nil)
		if err != nil || originalErr != nil || len(want) == 0 || !reflect.DeepEqual(got, want) {
			t.Fatalf("kept outline differs for %U", tc.keep)
		}
		if got, err := font.LoadGlyph(nil, discard, fixed.I(16), nil); err != nil || len(got) != 0 {
			t.Fatalf("unused outline still present for %U", tc.discard)
		}
	}
	output := buildExportPdfBuffer([]pdfLine{pdfPlain("日本語😀", false)})
	if len(output) > 500_000 {
		t.Fatalf("short Unicode PDF is %d bytes", len(output))
	}
}
