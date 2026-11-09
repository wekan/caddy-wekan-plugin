package web

import (
	"context"
	"errors"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// The mail action's "Attachments", as Node.js WeKan's
// server/lib/ruleCardAttachments.js and ruleEmailAttachments.js: the
// triggering card's live attachments - never a linked card's, never an id an
// action names - as files of the mail, with a manifest in its text. At most
// 100 files and 8 MiB together. (WeKan also times out a read after 30
// seconds, for its S3 and GridFS stores; Weddy's are files in its own
// directory, read with a byte limit instead.) The card must
// be readable by the rule's user before the reads and after them, and its
// files and cover the same afterwards; otherwise no mail is sent.

const (
	ruleMailMaxAttachmentBytes = 8 << 20
	ruleMailMaxAttachments     = 100
	ruleMailManifestMax        = 256 << 10
)

var ruleMailContentType = regexp.MustCompile(`^[a-zA-Z0-9!#$&^_.+-]+/[a-zA-Z0-9!#$&^_.+-]+$`)

// ruleMailAttachmentName is WeKan's: the name's last path element, no line
// breaks or NUL, at most 255 characters, else "attachment".
func ruleMailAttachmentName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(name)
	if r := []rune(name); len(r) > 255 {
		name = string(r[:255])
	}
	if name == "" {
		return "attachment"
	}
	return name
}

// ruleMailAttachments is WeKan's prepareRuleCardAttachments: the files, and
// the manifest to add to the text.
func (s *Server) ruleMailAttachments(ctx context.Context, userID, boardID, cardID string) ([]mailAttachment, string, error) {
	coverOf := func() (string, bool) {
		var c struct {
			CoverID string `bson:"coverId"`
		}
		if !s.ruleMailCardReadable(ctx, userID, boardID, cardID) ||
			s.db.Collection("cards").FindOne(ctx, bson.M{"_id": eq(cardID)}).Decode(&c) != nil {
			return "", false
		}
		return c.CoverID, true
	}
	cover, ok := coverOf()
	if !ok {
		return nil, "", errors.New("rule-email-attachments-not-authorized")
	}
	readFiles := func() ([]bson.M, error) {
		cur, err := s.db.Collection("attachments").Find(ctx, bson.M{"meta.cardId": eq(cardID), "deletedAt": nil},
			options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
		if err != nil {
			return nil, err
		}
		var out []bson.M
		return out, cur.All(ctx, &out)
	}
	files, err := readFiles()
	if err != nil {
		return nil, "", err
	}
	if len(files) > ruleMailMaxAttachments {
		return nil, "", errors.New("rule-email-too-many-attachments")
	}
	signature := func(fs []bson.M) string {
		var b strings.Builder
		for _, f := range fs {
			b.WriteString(canonical(f))
		}
		return b.String()
	}
	before := signature(files)
	var out []mailAttachment
	var lines []string
	total := 0
	for _, raw := range files {
		meta, _ := plain(raw["meta"]).(map[string]any)
		if id, _ := meta["cardId"].(string); id != cardID {
			return nil, "", errors.New("rule-email-attachments-changed")
		}
		var a Attachment
		if b, err := bson.Marshal(raw); err != nil || bson.Unmarshal(b, &a) != nil {
			return nil, "", errors.New("rule-email-attachment-unavailable")
		}
		if _, ok := coverOf(); !ok {
			return nil, "", errors.New("rule-email-attachments-not-authorized")
		}
		path, err := s.attachmentFile(&a)
		if err != nil {
			return nil, "", errors.New("rule-email-attachment-unavailable")
		}
		data, err := func() ([]byte, error) {
			f, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			// One byte over the room left is enough to know it is too much.
			data, err := io.ReadAll(io.LimitReader(f, int64(ruleMailMaxAttachmentBytes-total)+1))
			return data, err
		}()
		if err != nil {
			return nil, "", errors.New("rule-email-attachment-unavailable")
		}
		total += len(data)
		if total > ruleMailMaxAttachmentBytes {
			return nil, "", errors.New("rule-email-attachments-too-large")
		}
		name := ruleMailAttachmentName(a.Name)
		typ := a.Type
		if !ruleMailContentType.MatchString(typ) {
			typ = "application/octet-stream"
		}
		out = append(out, mailAttachment{Filename: name, ContentType: typ, Data: data})
		lines = append(lines, "File: "+name, "Size (bytes): "+strconv.Itoa(len(data)), "Type: "+typ)
		if cover != "" && a.ID == cover {
			lines = append(lines, "Cover: true")
		}
		for _, k := range []string{"uploadedAt", "uploadedAtOstrio", "createdAt"} {
			if t, ok := plain(raw[k]).(time.Time); ok && !t.IsZero() {
				lines = append(lines, "Uploaded: "+t.UTC().Format("2006-01-02T15:04:05.000Z"))
				break
			}
		}
		uploader := a.UserID
		if uploader == "" {
			uploader, _ = meta["userId"].(string)
		}
		if uploader != "" {
			lines = append(lines, "Uploaded by: "+s.ruleMailDisplayName(ctx, uploader))
		}
		lines = append(lines, "")
	}
	manifest := ""
	if len(lines) > 0 {
		manifest = "Attachments:\n" + strings.TrimRight(strings.Join(lines, "\n"), "\n")
	}
	if len(manifest) > ruleMailManifestMax {
		return nil, "", errors.New("rule-email-attachment-manifest-too-large")
	}
	after, err := readFiles()
	if err != nil || signature(after) != before {
		return nil, "", errors.New("rule-email-attachments-changed")
	}
	if now, ok := coverOf(); !ok {
		return nil, "", errors.New("rule-email-attachments-not-authorized")
	} else if now != cover {
		return nil, "", errors.New("rule-email-attachments-changed")
	}
	return out, manifest, nil
}
