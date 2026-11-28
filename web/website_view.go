package web

import (
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type websiteCard struct {
	Card
	Parent, URL, Date string
	Children          []*websiteCard
	when              time.Time
	listSort          float64
}

var websitePublished = []string{"published", "publish", "public", "live", "julkaistu", "julkinen", "veröffentlicht", "publié", "publicado", "pubblicato"}
var websitePageLanes = []string{"page", "pages", "sivut", "seiten", "pagine", "páginas"}

func websiteLocation(b *Board, page string) string {
	params := url.Values{"view": {"website"}}
	if page != "" {
		params.Set("page", page)
	}
	return "/b/" + b.ID + "/" + b.Slug + "?" + params.Encode()
}

func (s *Server) websiteRequested(r *http.Request, b *Board) bool {
	view := r.URL.Query().Get("view")
	if view != "" || r.PathValue("card") != "" {
		return view == "website"
	}
	side := "private"
	if b.Permission == "public" {
		side = "public"
	}
	var doc bson.M
	_ = s.db.Collection("boards").FindOne(r.Context(), bson.M{"_id": eq(b.ID)},
		options.FindOne().SetProjection(bson.M{defaultField(side): 1})).Decode(&doc)
	return boardViewStateOf(doc).defaultView(side) == "board-view-website"
}

func (s *Server) boardWebsite(w http.ResponseWriter, r *http.Request, b *Board, u *User, lanes []lane, lists []List) {
	ctx := r.Context()
	lang := languageOf(r, u).Tag
	published := map[string]bool{}
	for _, list := range lists {
		if slices.Contains(websitePublished, strings.ToLower(strings.TrimSpace(list.Title))) {
			published[list.ID] = true
		}
	}
	meta := map[string]bson.M{}
	for _, doc := range s.findDocs(ctx, "cards", bson.M{"boardId": eq(b.ID), "archived": false, "deletedAt": nil},
		options.Find().SetProjection(bson.M{"parentId": 1, "receivedAt": 1, "startAt": 1})) {
		meta[strOf(doc["_id"])] = bson.M(doc)
	}
	var pages, posts []*websiteCard
	visible := map[string]*websiteCard{}
	for _, lane := range lanes {
		isPage := slices.Contains(websitePageLanes, strings.ToLower(strings.TrimSpace(lane.Title)))
		for _, list := range lane.Lists {
			if len(published) > 0 && !published[list.ID] {
				continue
			}
			for _, card := range list.Cards {
				if card.Type == "template-container" || (u != nil && assignedOnlyOn(b, u) && !slices.Contains(card.Assignees, u.ID)) {
					continue
				}
				entry := &websiteCard{Card: card, Parent: strOf(meta[card.ID]["parentId"]), URL: websiteLocation(b, card.ID), listSort: list.Sort}
				for _, field := range []string{"receivedAt", "startAt"} {
					if when := isoDate(meta[card.ID][field]); when != "" {
						entry.when, _ = time.Parse(time.RFC3339Nano, when)
						if !entry.when.IsZero() {
							break
						}
					}
				}
				if entry.when.IsZero() {
					entry.when = card.CreatedAt
				}
				if !entry.when.IsZero() {
					entry.Date = dayOf(entry.when)
				}
				visible[card.ID] = entry
				if isPage {
					pages = append(pages, entry)
				} else {
					posts = append(posts, entry)
				}
			}
		}
	}
	before := func(a, b *websiteCard) bool {
		if a.listSort != b.listSort {
			return a.listSort < b.listSort
		}
		if a.Sort != b.Sort {
			return a.Sort < b.Sort
		}
		return a.Title < b.Title
	}
	sort.SliceStable(pages, func(i, j int) bool { return before(pages[i], pages[j]) })
	sort.SliceStable(posts, func(i, j int) bool {
		if !posts[i].when.Equal(posts[j].when) {
			return posts[i].when.After(posts[j].when)
		}
		return before(posts[i], posts[j])
	})
	var menu []*websiteCard
	for _, page := range pages {
		if parent := visible[page.Parent]; parent == nil {
			menu = append(menu, page)
		} else {
			parent.Children = append(parent.Children, page)
		}
	}
	selected := visible[r.URL.Query().Get("page")]
	if selected == nil && len(posts) == 0 && len(menu) > 0 {
		selected = menu[0]
	}
	data := map[string]any{"Title": b.Title, "Board": b, "User": u, "Home": websiteLocation(b, ""),
		"Menu": menu, "Posts": posts, "Page": selected, "Views": s.boardViewEntries(b, lang, "website")}
	if selected != nil {
		data["Body"] = viewer(selected.Description, s.textPolicy(ctx))
		var images, files []map[string]string
		for _, attachment := range s.cardAttachments(r, b, selected.ID) {
			link := map[string]string{"Name": attachment.Name, "URL": attachmentURL(attachment.ID)}
			if attachment.IsImage && strings.HasPrefix(attachment.Type, "image/") && inline[attachment.Type] {
				images = append(images, link)
			} else {
				files = append(files, link)
			}
		}
		data["Images"], data["Files"] = images, files
	}
	s.render(w, r, "website", http.StatusOK, data)
}
