package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// Admin Panel / Attachments over the REST API, as Node.js WeKan's
// server/models/attachmentStorageSettings.js: GET and PUT
// /api/admin/attachment-settings, for a site administrator only, on WeKan's
// own attachmentStorageSettings document. The first read makes the document
// with WeKan's defaults. Cloud-storage secrets are never returned - each is
// '' with a <field>Set marker - and a secret left blank in an update keeps
// the stored one. WeKan validates an update with the collection's schema;
// Weddy takes only the keys and types that schema names, and refuses the rest.

var storageSecretFields = map[string][]string{
	"s3": {"secretAccessKey"}, "azure": {"accountKey", "connectionString"}, "gcs": {"credentials"}, "webdav": {"password"},
}

var storageNames = []string{"filesystem", "gridfs", "s3", "azure", "gcs", "webdav"}

func envNonNegative(name string) int64 {
	if n, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(name)), 10, 64); err == nil && n >= 0 {
		return n
	}
	return 0
}

// attachmentSettingsDefaults is WeKan's first settings document.
func (s *Server) attachmentSettingsDefaults(userID string) bson.M {
	mimes := bson.A{}
	if v := os.Getenv("ATTACHMENTS_UPLOAD_MIME_TYPES"); v != "" {
		for _, t := range strings.Split(v, ",") {
			mimes = append(mimes, strings.TrimSpace(t))
		}
	}
	now := time.Now().UTC()
	return bson.M{
		"_id":            randomID(), // inserted, not a filter
		"defaultStorage": "filesystem",
		"storageConfig": bson.M{
			"filesystem": bson.M{"enabled": true, "read": true, "write": true, "path": filepath.Join(s.FilesDir, "attachments")},
			"gridfs":     bson.M{"enabled": true, "read": true, "write": true},
			"s3": bson.M{"enabled": false, "read": true, "write": true, "endpoint": os.Getenv("S3_ENDPOINT"), "region": os.Getenv("S3_REGION"),
				"bucket": os.Getenv("S3_BUCKET"), "accessKeyId": os.Getenv("S3_ACCESS_KEY"), "secretAccessKey": os.Getenv("S3_SECRET_KEY"), "forcePathStyle": true},
			"azure":  bson.M{"enabled": false, "read": true, "write": true, "accountName": "", "accountKey": "", "connectionString": "", "bucket": ""},
			"gcs":    bson.M{"enabled": false, "read": true, "write": true, "projectId": "", "keyFilename": "", "credentials": "", "bucket": ""},
			"webdav": bson.M{"enabled": false, "read": true, "write": true, "endpoint": "", "username": "", "password": "", "bucket": "wekan"}, // inserted, not a filter
		},
		"uploadSettings": bson.M{"maxFileSize": envNonNegative("ATTACHMENTS_UPLOAD_MAX_SIZE"), "allowedMimeTypes": mimes},
		"limitSettings": bson.M{"attachmentsUploadMaxBytes": envNonNegative("ATTACHMENTS_UPLOAD_MAX_SIZE"), "attachmentsUploadBlocked": false,
			"avatarsUploadBlocked": false, "attachmentsDownloadMaxBytes": int64(0), "attachmentsDownloadBlocked": false,
			"apiUploadMaxBytes": envNonNegative("ATTACHMENT_API_MAX_UPLOAD_BYTES"), "apiUploadBlocked": false,
			"apiDownloadMaxBytes": envNonNegative("ATTACHMENT_API_MAX_DOWNLOAD_BYTES"), "apiDownloadBlocked": false},
		"migrationSettings": bson.M{"autoMigrate": false, "batchSize": 10, "delayMs": 1000, "cpuThreshold": 70},
		"createdBy":         userID, "updatedBy": userID, "createdAt": now, "updatedAt": now,
	}
}

// maskStorageSecrets is WeKan's: each secret ”, with a <field>Set marker.
func maskStorageSecrets(doc map[string]any) map[string]any {
	cfg, _ := doc["storageConfig"].(map[string]any)
	for provider, fields := range storageSecretFields {
		p, _ := cfg[provider].(map[string]any)
		if p == nil {
			continue
		}
		for _, f := range fields {
			v, _ := p[f].(string)
			p[f+"Set"] = v != ""
			p[f] = ""
		}
	}
	return doc
}

func (s *Server) attachmentSettingsDoc(ctx context.Context, userID string) (map[string]any, error) {
	var raw bson.M
	coll := s.db.Collection("attachmentStorageSettings")
	if err := coll.FindOne(ctx, bson.M{}).Decode(&raw); err != nil {
		if _, err := coll.InsertOne(ctx, s.attachmentSettingsDefaults(userID)); err != nil {
			return nil, err
		}
		if err := coll.FindOne(ctx, bson.M{}).Decode(&raw); err != nil {
			return nil, err
		}
	}
	doc, _ := plain(raw).(map[string]any)
	return maskStorageSecrets(doc), nil
}

func (s *Server) adminAttachmentSettingsRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/admin/attachment-settings", s.api(s.apiGetAttachmentSettings))
	m.HandleFunc("PUT /api/admin/attachment-settings", s.api(s.apiPutAttachmentSettings))
}

func (s *Server) apiGetAttachmentSettings(w http.ResponseWriter, r *http.Request, u *User) {
	if !u.IsAdmin {
		apiError(w, http.StatusForbidden, "Admin access required")
		return
	}
	doc, err := s.attachmentSettingsDoc(r.Context(), u.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// cleanAttachmentSettings takes only what WeKan's schema names: errors name
// the first key or value it refuses.
func cleanAttachmentSettings(in map[string]any) (bson.M, error) {
	out := bson.M{}
	boolOK := func(v any) bool { _, ok := v.(bool); return ok }
	countOK := func(v any) bool { n, ok := v.(float64); return ok && n >= 0 && n == float64(int64(n)) && n < 1e15 }
	textOK := func(v any) bool { s, ok := v.(string); return ok && len(s) <= 4096 }
	for k, v := range in {
		switch k {
		case "_id", "createdAt", "updatedAt", "createdBy", "updatedBy":
			continue // WeKan's metadata, stripped
		case "defaultStorage":
			if s, ok := v.(string); !ok || !slices.Contains(storageNames, s) {
				return nil, errors.New("defaultStorage is one of " + strings.Join(storageNames, ", "))
			}
			out[k] = v
		case "storageConfig":
			cfg, ok := v.(map[string]any)
			if !ok {
				return nil, errors.New("storageConfig is an object")
			}
			clean := bson.M{}
			for provider, pv := range cfg {
				p, ok := pv.(map[string]any)
				if !ok || !slices.Contains(storageNames, provider) {
					return nil, errors.New("storageConfig." + provider + " is not a storage")
				}
				cp := bson.M{}
				for f, fv := range p {
					switch {
					case f == "enabled" || f == "read" || f == "write" || f == "forcePathStyle":
						if !boolOK(fv) {
							return nil, errors.New("storageConfig." + provider + "." + f + " is true or false")
						}
					case strings.HasSuffix(f, "Set"):
						continue // the client's marker, never stored
					case ruleFieldName.MatchString(f) && textOK(fv):
					default:
						return nil, errors.New("storageConfig." + provider + "." + f + " is not a setting")
					}
					cp[f] = fv
				}
				clean[provider] = cp
			}
			out[k] = clean
		case "uploadSettings", "limitSettings", "migrationSettings":
			m, ok := v.(map[string]any)
			if !ok {
				return nil, errors.New(k + " is an object")
			}
			clean := bson.M{}
			for f, fv := range m {
				valid := false
				switch k + "." + f {
				case "uploadSettings.maxFileSize", "limitSettings.attachmentsUploadMaxBytes", "limitSettings.attachmentsDownloadMaxBytes",
					"limitSettings.apiUploadMaxBytes", "limitSettings.apiDownloadMaxBytes",
					"migrationSettings.batchSize", "migrationSettings.delayMs", "migrationSettings.cpuThreshold":
					valid = countOK(fv)
				case "limitSettings.attachmentsUploadBlocked", "limitSettings.avatarsUploadBlocked", "limitSettings.attachmentsDownloadBlocked",
					"limitSettings.apiUploadBlocked", "limitSettings.apiDownloadBlocked", "migrationSettings.autoMigrate":
					valid = boolOK(fv)
				case "uploadSettings.allowedMimeTypes":
					list, ok := fv.([]any)
					valid = ok && len(list) <= 200
					for _, e := range list {
						if !textOK(e) {
							valid = false
						}
					}
				}
				if !valid {
					return nil, errors.New(k + "." + f + " is not a setting of that kind")
				}
				clean[f] = fv
			}
			out[k] = clean
		default:
			return nil, errors.New(k + " is not a setting")
		}
	}
	return out, nil
}

func (s *Server) apiPutAttachmentSettings(w http.ResponseWriter, r *http.Request, u *User) {
	if !u.IsAdmin {
		apiError(w, http.StatusForbidden, "Admin access required")
		return
	}
	var in map[string]any
	if err := decodeJSON(r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	clean, err := cleanAttachmentSettings(in)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	if _, err := s.attachmentSettingsDoc(ctx, u.ID); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var existing bson.M
	_ = s.db.Collection("attachmentStorageSettings").FindOne(ctx, bson.M{}).Decode(&existing)
	prev, _ := plain(existing).(map[string]any)
	// storageConfig: the providers given, merged over the stored ones; a
	// cloud secret left blank keeps the stored one.
	if cfg, ok := clean["storageConfig"].(bson.M); ok {
		prevCfg, _ := prev["storageConfig"].(map[string]any)
		merged := bson.M{}
		for k, v := range prevCfg {
			merged[k] = v
		}
		for provider, v := range cfg {
			incoming := v.(bson.M)
			prevP, _ := prevCfg[provider].(map[string]any)
			p := bson.M{}
			for k, v := range prevP {
				p[k] = v
			}
			for k, v := range incoming {
				p[k] = v
			}
			for _, f := range storageSecretFields[provider] {
				if s, _ := incoming[f].(string); s == "" {
					p[f], _ = prevP[f].(string)
				}
			}
			merged[provider] = p
		}
		clean["storageConfig"] = merged
	}
	clean["updatedBy"], clean["updatedAt"] = u.ID, time.Now().UTC()
	if _, err := s.db.Collection("attachmentStorageSettings").UpdateOne(ctx, bson.M{}, bson.M{"$set": clean}, options.UpdateOne().SetUpsert(true)); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	doc, err := s.attachmentSettingsDoc(ctx, u.ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, doc)
}
