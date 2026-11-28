package web

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

var internalHostPattern = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)

func cleanWebhookInternalHosts(text string) ([]string, bool) {
	hosts := make([]string, 0)
	for _, raw := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	}) {
		host, port := raw, ""
		if strings.HasPrefix(raw, "[") {
			end := strings.IndexByte(raw, ']')
			if end < 0 {
				return nil, false
			}
			ip, err := netip.ParseAddr(raw[1:end])
			if err != nil || !ip.Is6() || ip.Zone() != "" {
				return nil, false
			}
			host = "[" + ip.String() + "]"
			if suffix := raw[end+1:]; suffix != "" {
				var ok bool
				port, ok = strings.CutPrefix(suffix, ":")
				if !ok || port == "" {
					return nil, false
				}
			}
		} else {
			var found bool
			host, port, found = strings.Cut(raw, ":")
			if (found && port == "") || len(host) > 253 || !internalHostPattern.MatchString(host) {
				return nil, false
			}
		}
		if port != "" {
			for _, digit := range port {
				if digit < '0' || digit > '9' {
					return nil, false
				}
			}
			number, err := strconv.ParseUint(port, 10, 16)
			if err != nil || number == 0 || len(port) > 5 {
				return nil, false
			}
			host += ":" + strconv.FormatUint(number, 10)
		}
		if !slices.Contains(hosts, host) {
			hosts = append(hosts, host)
		}
		if len(hosts) > 50 {
			return nil, false
		}
	}
	return hosts, true
}

func (s *Server) readWebhookInternalHosts(ctx context.Context) ([]string, error) {
	var doc struct {
		Hosts []string `bson:"webhookInternalHosts"`
	}
	err := s.db.Collection("settings").FindOne(ctx, bson.M{}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		err = nil
	}
	return doc.Hosts, err
}

func (s *Server) saveWebhookInternalHosts(w http.ResponseWriter, r *http.Request, u *User) {
	if !u.IsAdmin {
		http.NotFound(w, r)
		return
	}
	if s.refuseWebhookWrite(r, u, "saveWebhookInternalHosts", "Changed webhook internal destinations while webhooks are turned off.") {
		webhooksOffAnswer(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid internal destinations", http.StatusBadRequest)
		return
	}
	hosts, ok := cleanWebhookInternalHosts(r.PostForm.Get("hosts"))
	if !ok {
		http.Error(w, "Invalid internal destination", http.StatusBadRequest)
		return
	}
	result, err := s.db.Collection("settings").UpdateOne(r.Context(), bson.M{"webhooksDisabled": bson.M{"$ne": true}},
		bson.M{"$set": bson.M{"webhookInternalHosts": hosts}})
	if err != nil || result.MatchedCount != 1 {
		http.Error(w, "Could not save internal destinations", http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/admin/settings/global-webhooks", http.StatusSeeOther)
}
