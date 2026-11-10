package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// A board, or one card, as Excel (.xlsx) and as PDF, as Node.js WeKan's
// exportExcel and exportPDF routes answer them - with the same rule as its
// other exports (models/lib/exportAccess.js): a board its reader may see, and
// never all of a board for an assigned-only member, who exports only a card
// assigned to them. Written with the standard library only: an .xlsx is a ZIP
// of a few XML parts, and the PDF is plain text pages in a built-in font.

const xlsxType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

func (s *Server) exportOfficeRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/boards/{board}/exportExcel", s.exportable(s.apiExportOffice("xlsx", false)))
	m.HandleFunc("GET /api/boards/{board}/exportPDF", s.exportable(s.apiExportOffice("pdf", false)))
	m.HandleFunc("GET /api/boards/{board}/lists/{list}/cards/{card}/exportExcel", s.exportable(s.apiExportOffice("xlsx", true)))
	m.HandleFunc("GET /api/boards/{board}/lists/{list}/cards/{card}/exportPDF", s.exportable(s.apiExportOffice("pdf", true)))
}

// exportReader is the request's board and whether its reader may export it -
// all of it, or (oneCard) the card given; it answers the refusal itself.
func (s *Server) exportReader(w http.ResponseWriter, r *http.Request, oneCard bool) (*Board, *Card, bool) {
	var b Board
	if id := r.PathValue("board"); !validID(id) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(id)}).Decode(&b) != nil {
		apiError(w, http.StatusNotFound, "Not found")
		return nil, nil, false
	}
	u, ok := s.exportUser(r)
	if !ok {
		apiError(w, http.StatusUnauthorized, "Invalid token")
		return nil, nil, false
	}
	var c *Card
	if oneCard {
		var card Card
		id, list := r.PathValue("card"), r.PathValue("list")
		if !validID(id) || !validID(list) || s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(id), "listId": eq(list), "boardId": b.ID}).Decode(&card) != nil {
			apiError(w, http.StatusNotFound, "Not found")
			return nil, nil, false
		}
		c = &card
	}
	if b.Permission == "public" {
		return &b, c, true
	}
	if u == nil {
		apiError(w, http.StatusUnauthorized, "Unauthorized")
		return nil, nil, false
	}
	if m := b.member(u.ID); m == nil || (m.AssignedOnly() && (c == nil || !slices.Contains(c.Assignees, u.ID))) {
		apiError(w, http.StatusForbidden, "Forbidden")
		return nil, nil, false
	}
	return &b, c, true
}

func (s *Server) apiExportOffice(format string, oneCard bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// models/exportExcel.js and exportPDF.js: a board that is not there
		// is plain text, and no file.
		if id := r.PathValue("board"); !validID(id) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(id)}).Err() != nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("Board not found"))
			return
		}
		b, c, ok := s.exportReader(w, r, oneCard)
		if !ok {
			return
		}
		ctx := r.Context()
		name := "export-board-" + b.ID
		if c != nil {
			name = "export-card-" + c.ID
		}
		w.Header().Set("Cache-Control", "no-store")
		if format == "xlsx" {
			var out []byte
			var err error
			// WeKan's workbooks, their words in the user's language, else
			// ?lang=, else English (exportExcel.js): a card's in the card
			// layout (ExporterExcelCard); a board's in the table layout with
			// Card details unticked, else in the card layout.
			{
				q := r.URL.Query()
				u, _ := s.exportUser(r)
				lang := "en"
				if u != nil && u.Profile.Language != "" {
					lang = u.Profile.Language
				} else if l := q.Get("lang"); l != "" && len(l) <= 32 {
					lang = l
				}
				// adminOnlyCustomFields.js assertFieldExport: a board with
				// admin-only custom fields is exported by its admins only.
				if s.boardHasAdminOnlyFields(ctx, b) && (u == nil || !b.AdminOf(u.ID)) {
					apiError(w, http.StatusForbidden, "This export requires board administrator access")
					return
				}
				want := exportWanted(q.Get("fields"))
				if c == nil && want != nil && !want["card-details"] {
					out, err = s.excelTableWorkbook(ctx, b, lang)
				} else {
					o := excelCardOptions{Lang: lang, DateFormat: "YYYY-MM-DD"}
					if u != nil && u.Profile.DateFormat != "" {
						o.DateFormat = u.Profile.DateFormat
					}
					if f := q.Get("dateFormat"); slices.Contains(excelDateFormats, f) {
						o.DateFormat = f
					}
					if tz := q.Get("tz"); len(tz) <= 64 {
						o.Zone = tz
					}
					if want != nil {
						for _, k := range exportFieldKeys {
							if want[k] {
								o.Fields = append(o.Fields, k)
							}
						}
					}
					out, err = s.excelBoardWorkbook(ctx, b, o, c)
				}
			}
			if err != nil {
				apiError(w, http.StatusInternalServerError, "Export failed")
				return
			}
			w.Header().Set("Content-Type", xlsxType)
			w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.xlsx"`)
			_, _ = w.Write(out)
			return
		}
		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.pdf"`)
		_, _ = w.Write(pdfOf(s.pdfLines(ctx, b, c)))
	}
}

// cellText is a cell as text for a sheet or a page.
func cellText(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return strings.Trim(csvCell(v), `"`)
}

// xlsxOf is a one-sheet workbook of rows, every cell an inline string.
func xlsxOf(title string, rows [][]any) ([]byte, error) {
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	col := func(i int) string {
		name := ""
		for i++; i > 0; i = (i - 1) / 26 {
			name = string(rune('A'+(i-1)%26)) + name
		}
		return name
	}
	var sheet strings.Builder
	sheet.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` +
		`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for i, row := range rows {
		fmt.Fprintf(&sheet, `<row r="%d">`, i+1)
		for j, cell := range row {
			fmt.Fprintf(&sheet, `<c r="%s%d" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, col(j), i+1, esc(cellText(cell)))
		}
		sheet.WriteString(`</row>`)
	}
	sheet.WriteString(`</sheetData></worksheet>`)
	sheetName := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`[]:*?/\`, r) {
			return '_'
		}
		return r
	}, title)
	if sheetName == "" {
		sheetName = "Board"
	}
	if len([]rune(sheetName)) > 31 {
		sheetName = string([]rune(sheetName)[:31])
	}
	parts := map[string]string{
		"[Content_Types].xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/>` +
			`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
			`<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/></Types>`,
		"_rels/.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`,
		"xl/workbook.xml": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" ` +
			`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="` + esc(sheetName) + `" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`,
		"xl/worksheets/sheet1.xml": sheet.String(),
	}
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for _, name := range []string{"[Content_Types].xml", "_rels/.rels", "xl/workbook.xml", "xl/_rels/workbook.xml.rels", "xl/worksheets/sheet1.xml"} {
		f, err := z.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write([]byte(parts[name])); err != nil {
			return nil, err
		}
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// pdfLines are what the PDF says: the board, then each list with its cards -
// or the one card with its list and description.
func (s *Server) pdfLines(ctx context.Context, b *Board, c *Card) []string {
	// Admin Panel -> Anonymize export users: the @mentions of what it prints.
	a := s.exportAnonymizer(ctx, b, nil)
	lines := []string{b.Title, ""}
	if c != nil {
		lines = append(lines, a.text(c.Title), s.titleOfID(ctx, "lists", c.ListID), "")
		lines = append(lines, strings.Split(a.text(c.Description), "\n")...)
		return append(lines, s.cardPDFDetails(ctx, b, c, a)...)
	}
	cur, err := s.db.Collection("lists").Find(ctx, bson.M{"boardId": b.ID, "archived": false}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	if err != nil {
		return lines
	}
	var lists []List
	_ = cur.All(ctx, &lists)
	for _, l := range lists {
		lines = append(lines, l.Title)
		cc, err := s.db.Collection("cards").Find(ctx, bson.M{"boardId": b.ID, "listId": l.ID, "archived": false}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
		if err == nil {
			var cards []Card
			_ = cc.All(ctx, &cards)
			for _, x := range cards {
				lines = append(lines, "    - "+a.text(x.Title))
			}
		}
		lines = append(lines, "")
	}
	return lines
}

// pdfText is a line in a PDF string: Latin-1, the rest as "?", and ( ) \ escaped.
func pdfText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '(' || r == ')' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\t':
			b.WriteString("    ")
		case r < 32:
		case r < 256:
			b.WriteByte(byte(r))
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

// pdfOf is a PDF of lines on A4 pages in Helvetica, 50 lines a page.
func pdfOf(lines []string) []byte {
	const perPage = 50
	var pages [][]string
	for len(lines) > perPage {
		pages = append(pages, lines[:perPage])
		lines = lines[perPage:]
	}
	pages = append(pages, lines)
	var objs []string
	// 1 catalog, 2 pages, 3 font, then a page and its contents each.
	kids := []string{}
	for i := range pages {
		kids = append(kids, strconv.Itoa(4+2*i)+" 0 R")
	}
	objs = append(objs, "<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids ["+strings.Join(kids, " ")+"] /Count "+strconv.Itoa(len(pages))+" >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")
	for i, page := range pages {
		var content strings.Builder
		content.WriteString("BT /F1 11 Tf 50 800 Td 15 TL\n")
		for _, l := range page {
			content.WriteString("(" + pdfText(l) + ") Tj T*\n")
		}
		content.WriteString("ET")
		objs = append(objs, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 3 0 R >> >> /Contents "+strconv.Itoa(5+2*i)+" 0 R >>",
			"<< /Length "+strconv.Itoa(content.Len())+" >>\nstream\n"+content.String()+"\nendstream")
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

// cardPDFDetails are a card's checklists, each item ticked or not, and its
// comments with their authors - as WeKan's ExporterCardPDF prints them, the
// names anonymized when Admin Panel says so.
func (s *Server) cardPDFDetails(ctx context.Context, b *Board, c *Card, a *anonMap) []string {
	lang := exportLang(ctx)
	var lines []string
	cur, err := s.db.Collection("checklists").Find(ctx, bson.M{"cardId": c.ID}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
	if err == nil {
		var lists []bson.M
		_ = cur.All(ctx, &lists)
		if len(lists) > 0 {
			lines = append(lines, "", tr(lang, "checklists"))
		}
		for _, cl := range lists {
			lines = append(lines, a.text(strOf(cl["title"])))
			ic, err := s.db.Collection("checklistItems").Find(ctx, bson.M{"checklistId": eq(strOf(cl["_id"]))}, options.Find().SetSort(bson.D{{Key: "sort", Value: 1}}))
			if err != nil {
				continue
			}
			var items []bson.M
			_ = ic.All(ctx, &items)
			for _, it := range items {
				box := "[ ]"
				if it["isFinished"] == true {
					box = "[x]"
				}
				lines = append(lines, "    "+box+" "+a.text(strOf(it["title"])))
			}
		}
	}
	cc, err := s.db.Collection("card_comments").Find(ctx, bson.M{"cardId": c.ID, "boardId": b.ID}, options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}}))
	if err != nil {
		return lines
	}
	var comments []bson.M
	_ = cc.All(ctx, &comments)
	if len(comments) == 0 {
		return lines
	}
	ids := bson.A{}
	for _, cm := range comments {
		ids = append(ids, cm["userId"])
	}
	users := []bson.M{}
	if uc, err := s.db.Collection("users").Find(ctx, bson.M{"_id": dbIn(ids)}, options.Find().SetProjection(bson.M{"username": 1})); err == nil {
		_ = uc.All(ctx, &users)
	}
	// The commenters too, members or not.
	if x := s.exportAnonymizer(ctx, b, users); x != nil {
		a = x
	}
	names := map[string]string{}
	for _, u := range users {
		a.user(u)
		names[strOf(u["_id"])] = strOf(u["username"])
	}
	lines = append(lines, "", tr(lang, "comments"))
	for _, cm := range comments {
		for i, l := range strings.Split(a.text(strOf(cm["text"])), "\n") {
			if i == 0 {
				l = names[strOf(cm["userId"])] + ": " + l
			} else {
				l = "    " + l
			}
			lines = append(lines, l)
		}
	}
	return lines
}
