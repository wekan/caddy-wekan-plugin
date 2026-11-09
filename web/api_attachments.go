package web

import (
	"encoding/base64"
	"net/http"
	"os"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Attachments at the board's address, as Node.js WeKan
// (server/models/boards.js, server/models/attachments.js, models/export.js):
// listed, the deleted ones listed, soft-deleted and restored, one exported.
func (s *Server) apiBoardAttachmentRoutes(m *http.ServeMux) {
	base := "/api/boards/{board}/attachments"
	m.HandleFunc("GET "+base, s.api(s.apiBoardAttachments(false)))
	m.HandleFunc("GET "+base+"/deleted", s.api(s.apiBoardAttachments(true)))
	m.HandleFunc("DELETE "+base+"/{id}", s.api(s.apiSoftDeleteAttachment))
	m.HandleFunc("POST "+base+"/{id}/restore", s.api(s.apiRestoreAttachment))
	m.HandleFunc("GET "+base+"/{id}/export", s.exportable(s.apiExportAttachment))
}

// attachmentLink is the address Meteor-Files gives an attachment.
func attachmentLink(a bson.M) string {
	id, _ := a["_id"].(string)
	ext, _ := a["extensionWithDot"].(string)
	return "/cdn/storage/attachments/" + id + "/original/" + id + ext
}

func (s *Server) apiBoardAttachments(deleted bool) apiHandler {
	return func(w http.ResponseWriter, r *http.Request, u *User) {
		b := s.apiBoardFor(w, r, u, false)
		if b == nil {
			return
		}
		filter := bson.M{"meta.boardId": b.ID, "deletedAt": nil}
		if deleted {
			filter["deletedAt"] = bson.M{"$ne": nil}
		}
		cur, err := s.db.Collection("attachments").Find(r.Context(), filter, options.Find().SetSort(bson.D{{Key: "uploadedAt", Value: 1}}))
		if err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		docs := []bson.M{}
		_ = cur.All(r.Context(), &docs)
		out := make([]map[string]any, 0, len(docs))
		for _, a := range docs {
			meta, _ := a["meta"].(bson.D)
			m := docMap(meta)
			e := map[string]any{"attachmentId": a["_id"], "attachmentName": a["name"], "attachmentType": a["type"],
				"boardId": m["boardId"], "swimlaneId": m["swimlaneId"], "listId": m["listId"], "cardId": m["cardId"]}
			if deleted {
				e["deletedAt"], e["deletedBy"], e["deleteBatchId"] = a["deletedAt"], a["deletedBy"], a["deleteBatchId"]
			} else {
				link := attachmentLink(a)
				e["url"], e["urlDownload"] = link, link+"?download=true&token="
			}
			out = append(out, e)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// boardAttachment is the request's attachment of the board, deleted or not, or 404.
func (s *Server) boardAttachment(w http.ResponseWriter, r *http.Request) *Attachment {
	id, board := r.PathValue("id"), r.PathValue("board")
	var a Attachment
	if !validID(id) || !validID(board) ||
		s.db.Collection("attachments").FindOne(r.Context(), bson.M{"_id": eq(id), "meta.boardId": eq(board)}).Decode(&a) != nil {
		apiError(w, http.StatusNotFound, "Attachment not found")
		return nil
	}
	return &a
}

// mayChangeAttachment is WeKan's canSoftDeleteAttachment: write access to the
// attachment's card (an assigned-only member's own card), or to its board.
func (s *Server) mayChangeAttachment(r *http.Request, u *User, a *Attachment) bool {
	var b Board
	if s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(a.Meta.BoardID)}).Decode(&b) != nil {
		return false
	}
	var c Card
	if a.Meta.CardID != "" && s.db.Collection("cards").FindOne(r.Context(), bson.M{"_id": eq(a.Meta.CardID)}).Decode(&c) == nil {
		return b.WritableIn(r.Context(), s, u, "write", &c)
	}
	return b.Writable(u, "write", nil)
}

func (s *Server) apiSoftDeleteAttachment(w http.ResponseWriter, r *http.Request, u *User) {
	a := s.boardAttachment(w, r)
	if a == nil {
		return
	}
	if !s.mayChangeAttachment(r, u, a) {
		apiError(w, http.StatusForbidden, "You do not have permission to delete this attachment")
		return
	}
	var raw bson.M
	_ = s.db.Collection("attachments").FindOne(r.Context(), bson.M{"_id": a.ID}).Decode(&raw)
	if raw["deletedAt"] != nil { // already deleted: the batch it was deleted in
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "batchId": raw["deleteBatchId"]})
		return
	}
	batch, err := s.softDeleteAttachmentBatch(r, a, u)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publish(a.Meta.BoardID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "batchId": batch})
}

func (s *Server) apiRestoreAttachment(w http.ResponseWriter, r *http.Request, u *User) {
	a := s.boardAttachment(w, r)
	if a == nil {
		return
	}
	if !s.mayChangeAttachment(r, u, a) {
		apiError(w, http.StatusForbidden, "You do not have permission to restore this attachment")
		return
	}
	if a.DeletedAt == nil {
		writeJSON(w, http.StatusOK, map[string]any{"restored": false})
		return
	}
	if err := s.restoreAttachment(r, a, u); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.hub.publish(a.Meta.BoardID, "board-changed")
	writeJSON(w, http.StatusOK, map[string]any{"restored": true})
}

// apiExportAttachment is WeKan's single-attachment export: the attachment with
// its file as base64, for whoever may export the board.
func (s *Server) apiExportAttachment(w http.ResponseWriter, r *http.Request) {
	var b Board
	if !validID(r.PathValue("board")) || s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(r.PathValue("board"))}).Decode(&b) != nil {
		apiError(w, http.StatusNotFound, "Not found")
		return
	}
	u, ok := s.exportUser(r)
	if !ok {
		apiError(w, http.StatusUnauthorized, "Invalid token")
		return
	}
	if b.Permission != "public" {
		if u == nil {
			apiError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		if m := b.member(u.ID); m == nil || m.AssignedOnly() {
			apiError(w, http.StatusForbidden, "Forbidden")
			return
		}
	}
	id := r.PathValue("id")
	var a Attachment
	if !validID(id) || s.db.Collection("attachments").FindOne(r.Context(), bson.M{"_id": eq(id), "meta.boardId": b.ID, "deletedAt": nil}).Decode(&a) != nil {
		writeJSON(w, http.StatusOK, map[string]any{}) // WeKan answers {} for no such attachment
		return
	}
	out := map[string]any{"_id": a.ID, "cardId": a.Meta.CardID, "name": a.Name, "type": a.Type, "file": nil}
	var raw bson.M
	_ = s.db.Collection("attachments").FindOne(r.Context(), bson.M{"_id": a.ID}).Decode(&raw)
	if meta, ok := raw["meta"].(bson.D); ok {
		out["source"] = docMap(meta)["source"]
	}
	if p, err := s.attachmentFile(&a); err == nil {
		if data, err := os.ReadFile(p); err == nil {
			out["file"] = base64.StdEncoding.EncodeToString(data)
		}
	}
	writeJSON(w, http.StatusOK, out)
}
