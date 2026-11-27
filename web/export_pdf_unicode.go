package web

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	_ "embed"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode/utf16"

	"golang.org/x/image/font"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// GNU Unifont 15.0.05, distributed with its license in pdf_fonts.
//
//go:embed pdf_fonts/unifont.ttf.gz
var bundledPDFUnifont []byte

type pdfFontProgram struct {
	face       *sfnt.Font
	compressed []byte
	size       int
}

var loadPDFUnifont = sync.OnceValues(func() (pdfFontProgram, error) {
	reader, err := gzip.NewReader(bytes.NewReader(bundledPDFUnifont))
	if err != nil {
		return pdfFontProgram{}, err
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return pdfFontProgram{}, err
	}
	face, err := sfnt.Parse(data)
	if err != nil {
		return pdfFontProgram{}, err
	}
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := writer.Write(data); err != nil {
		return pdfFontProgram{}, err
	}
	if err := writer.Close(); err != nil {
		return pdfFontProgram{}, err
	}
	return pdfFontProgram{face, compressed.Bytes(), len(data)}, nil
})

type pdfUnicodeFont struct {
	face       *sfnt.Font
	buffer     sfnt.Buffer
	characters map[rune]uint16
	runes      []rune
	glyphs     []sfnt.GlyphIndex
	widths     []int
}

func (f *pdfUnicodeFont) encode(text string) string {
	var encoded strings.Builder
	for _, r := range text {
		cid, exists := f.characters[r]
		if !exists && len(f.runes) < 65535 {
			glyph, _ := f.face.GlyphIndex(&f.buffer, r)
			advance, _ := f.face.GlyphAdvance(&f.buffer, glyph, fixed.I(1000), font.HintingNone)
			f.runes = append(f.runes, r)
			f.glyphs = append(f.glyphs, glyph)
			f.widths = append(f.widths, advance.Round())
			cid = uint16(len(f.runes))
			f.characters[r] = cid
		}
		fmt.Fprintf(&encoded, "%04X", cid)
	}
	return encoded.String()
}

func pdfStream(properties string, data []byte) []byte {
	header := fmt.Sprintf("<< /Length %d %s >>\nstream\n", len(data), properties)
	return append(append([]byte(header), data...), []byte("\nendstream")...)
}

func (f *pdfUnicodeFont) objects(add func([]byte) int) []byte {
	unifont, _ := loadPDFUnifont()
	program := add(pdfStream(fmt.Sprintf("/Filter /FlateDecode /Length1 %d", unifont.size), unifont.compressed))
	descriptor := add([]byte(fmt.Sprintf("<< /Type /FontDescriptor /FontName /Unifont /Flags 4 /FontBBox [0 -125 1000 875] /ItalicAngle 0 /Ascent 875 /Descent -125 /CapHeight 750 /StemV 80 /FontFile2 %d 0 R >>", program)))
	glyphMap := make([]byte, (len(f.glyphs)+1)*2)
	for i, glyph := range f.glyphs {
		binary.BigEndian.PutUint16(glyphMap[(i+1)*2:], uint16(glyph))
	}
	mapID := add(pdfStream("", glyphMap))
	var widths strings.Builder
	for _, width := range f.widths {
		fmt.Fprintf(&widths, " %d", width)
	}
	descendant := add([]byte(fmt.Sprintf("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /Unifont /CIDSystemInfo << /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> /FontDescriptor %d 0 R /CIDToGIDMap %d 0 R /W [1 [%s ]] >>", descriptor, mapID, widths.String())))
	var cmap strings.Builder
	cmap.WriteString("/CIDInit /ProcSet findresource begin\n12 dict begin\nbegincmap\n/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def\n/CMapName /WeddyUnicode def\n/CMapType 2 def\n1 begincodespacerange\n<0000> <FFFF>\nendcodespacerange\n")
	for start := 0; start < len(f.runes); start += 100 {
		end := min(start+100, len(f.runes))
		fmt.Fprintf(&cmap, "%d beginbfchar\n", end-start)
		for i := start; i < end; i++ {
			fmt.Fprintf(&cmap, "<%04X> <", i+1)
			for _, unit := range utf16.Encode([]rune{f.runes[i]}) {
				fmt.Fprintf(&cmap, "%04X", unit)
			}
			cmap.WriteString(">\n")
		}
		cmap.WriteString("endbfchar\n")
	}
	cmap.WriteString("endcmap\nCMapName currentdict /CMap defineresource pop\nend\nend\n")
	unicodeMap := add(pdfStream("", []byte(cmap.String())))
	return []byte(fmt.Sprintf("<< /Type /Font /Subtype /Type0 /BaseFont /Unifont /Encoding /Identity-H /DescendantFonts [%d 0 R] /ToUnicode %d 0 R >>", descendant, unicodeMap))
}

func needsPDFUnicode(text string) bool {
	for _, r := range text {
		if r >= 32 {
			if _, ok := cp1252Char(r); !ok {
				return true
			}
		}
	}
	return false
}
