package web

import (
	"encoding/binary"
	"math/bits"
	"slices"

	"golang.org/x/image/font/sfnt"
)

// Retain original glyph IDs for the PDF's CIDToGIDMap. Blank glyph entries
// have no outline bytes. Composite glyphs keep their component outlines.
func subsetPDFTrueType(source []byte, used []sfnt.GlyphIndex, glyphCount int) []byte {
	u16, u32 := binary.BigEndian.Uint16, binary.BigEndian.Uint32
	tables := make(map[string][]byte)
	for i := 0; i < int(u16(source[4:])); i++ {
		entry := source[12+i*16:]
		start, size := int(u32(entry[8:])), int(u32(entry[12:]))
		tables[string(entry[:4])] = source[start : start+size]
	}
	head, loca, glyf := slices.Clone(tables["head"]), tables["loca"], tables["glyf"]
	longLocations := u16(head[50:]) == 1
	offset := func(i int) int {
		if longLocations {
			return int(u32(loca[i*4:]))
		}
		return 2 * int(u16(loca[i*2:]))
	}
	keep := make([]bool, glyphCount)
	var mark func(int)
	mark = func(id int) {
		if keep[id] {
			return
		}
		keep[id] = true
		outline := glyf[offset(id):offset(id+1)]
		if len(outline) < 10 || int16(u16(outline)) >= 0 {
			return
		}
		for i := 10; ; {
			flags := u16(outline[i:])
			mark(int(u16(outline[i+2:])))
			i += 6
			if flags&1 != 0 {
				i += 2
			}
			switch {
			case flags&8 != 0:
				i += 2
			case flags&64 != 0:
				i += 4
			case flags&128 != 0:
				i += 8
			}
			if flags&32 == 0 {
				break
			}
		}
	}
	mark(0)
	for _, id := range used {
		mark(int(id))
	}
	newLoca := make([]byte, (glyphCount+1)*4)
	var newGlyf []byte
	for id, retain := range keep {
		binary.BigEndian.PutUint32(newLoca[id*4:], uint32(len(newGlyf)))
		if retain {
			newGlyf = append(newGlyf, glyf[offset(id):offset(id+1)]...)
		}
	}
	binary.BigEndian.PutUint32(newLoca[glyphCount*4:], uint32(len(newGlyf)))
	binary.BigEndian.PutUint16(head[50:], 1)
	clear(head[8:12])
	tables["head"], tables["loca"], tables["glyf"] = head, newLoca, newGlyf

	if post := tables["post"]; len(post) >= 32 {
		header := slices.Clone(post[:32])
		binary.BigEndian.PutUint32(header, 0x00030000) // no per-glyph names
		tables["post"] = header
	}

	// The PDF supplies widths, Unicode mappings and glyph names externally.
	// Keep the tables needed to load the outlines and to identify the font.
	tags := []string{"OS/2", "cmap", "cvt ", "fpgm", "gasp", "glyf", "head", "hhea", "hmtx", "loca", "maxp", "name", "post", "prep"}
	tags = slices.DeleteFunc(tags, func(tag string) bool { return len(tables[tag]) == 0 && tag != "glyf" })
	count := len(tags)
	searchRange := 16 << (bits.Len(uint(count)) - 1)
	output := make([]byte, 12+count*16)
	copy(output, source[:4])
	binary.BigEndian.PutUint16(output[4:], uint16(count))
	binary.BigEndian.PutUint16(output[6:], uint16(searchRange))
	binary.BigEndian.PutUint16(output[8:], uint16(bits.Len(uint(count))-1))
	binary.BigEndian.PutUint16(output[10:], uint16(count*16-searchRange))
	headOffset := 0
	for i, tag := range tags {
		data := tables[tag]
		start := len(output)
		output = append(output, data...)
		output = append(output, make([]byte, (4-len(data)%4)%4)...)
		entry := output[12+i*16:]
		copy(entry, tag)
		binary.BigEndian.PutUint32(entry[4:], pdfFontChecksum(output[start:]))
		binary.BigEndian.PutUint32(entry[8:], uint32(start))
		binary.BigEndian.PutUint32(entry[12:], uint32(len(data)))
		if tag == "head" {
			headOffset = start
		}
	}
	binary.BigEndian.PutUint32(output[headOffset+8:], 0xB1B0AFBA-pdfFontChecksum(output))
	return output
}

func pdfFontChecksum(data []byte) uint32 {
	var sum uint32
	for i := 0; i < len(data); i += 4 {
		sum += binary.BigEndian.Uint32(data[i:])
	}
	return sum
}
