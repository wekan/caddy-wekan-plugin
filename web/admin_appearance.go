package web

import (
	"net/http"
	"slices"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func (s *Server) adminAppearancePage(w http.ResponseWriter, r *http.Request, u *User) {
	if !u.IsAdmin {
		http.NotFound(w, r)
		return
	}
	var settings struct {
		ID   string         `bson:"_id"`
		Date siteDateFormat `bson:",inline"`
	}
	if err := s.db.Collection("settings").FindOne(r.Context(), bson.M{}).Decode(&settings); err != nil {
		http.Error(w, "Settings are unavailable", http.StatusInternalServerError)
		return
	}
	format := settings.Date.Format
	if !slices.Contains(dateFormats, format) {
		format = dateFormats[0]
	}
	var formats []dateOption
	for _, f := range dateFormats {
		formats = append(formats, dateOption{Value: f, Name: f, Selected: f == format})
	}
	lang := languageOf(r, u).Tag
	s.render(w, r, "adminappearance", http.StatusOK, map[string]any{
		"Title": tr(lang, "layout"), "User": u, "SettingID": settings.ID,
		"DateEnabled": settings.Date.Enabled, "DateFormats": formats,
	})
}
