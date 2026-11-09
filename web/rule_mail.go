package web

import (
	"context"
	"net/mail"
	"os"
	"regexp"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Rules' "send an email" action, as Node.js WeKan's prepareEmailAction
// (server/rulesHelper.js) and models/lib/ruleEmailMembers.js: To, the
// subject and the message with the rule's variables; the people tokens -
// {creator}, {assignees}, {members} - in To are their addresses, and an
// @username in To is that board member's own address, a non-member's
// dropped. The card's title, description and link close the message (#3301,
// #2713). On request, the card's details (rule_mail_details.go) and its
// checklists and comments (rule_mail_discussion.go) follow, in WeKan's
// order, and its attachments (rule_mail_attachments.go).

var mentionPart = regexp.MustCompile(`^@([A-Za-z0-9._-]{1,100})$`)

func (s *Server) ruleSendEmail(ctx context.Context, a, act bson.M, c *Card, vars map[string]string) error {
	var b Board
	if s.db.Collection("boards").FindOne(ctx, bson.M{"_id": eq(c.BoardID)}).Decode(&b) != nil {
		return nil
	}
	// The people tokens are addresses here.
	people := map[string]string{}
	for token, ids := range map[string][]string{"creator": {c.UserID}, "assignees": c.Assignees, "members": c.Members} {
		var addrs []string
		for _, id := range ids {
			var u User
			if validID(id) && s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(id)}).Decode(&u) == nil && len(u.Emails) > 0 && u.Emails[0].Address != "" {
				addrs = append(addrs, u.Emails[0].Address)
			}
		}
		people[token] = strings.Join(addrs, ", ")
	}
	toVars := map[string]string{}
	for k, v := range vars {
		toVars[k] = v
	}
	for k, v := range people {
		toVars[k] = v
	}
	var to []string
	seen := map[string]bool{}
	for _, part := range strings.Split(substituteVars(docString(act, "emailTo"), toVars), ",") {
		part = strings.TrimSpace(part)
		if m := mentionPart.FindStringSubmatch(part); m != nil {
			var u User
			part = ""
			if s.db.Collection("users").FindOne(ctx, bson.M{"username": eq(m[1])}).Decode(&u) == nil && b.MemberOf(u.ID) && len(u.Emails) > 0 {
				part = u.Emails[0].Address
			}
		}
		addr, err := mail.ParseAddress(part)
		if part == "" || err != nil || seen[strings.ToLower(addr.Address)] {
			continue
		}
		seen[strings.ToLower(addr.Address)] = true
		to = append(to, addr.Address)
	}
	if len(to) == 0 {
		return nil
	}
	body := substituteVars(docString(act, "emailMsg"), vars)
	subject := strings.Join(strings.Fields(substituteVars(docString(act, "emailSubject"), vars)), " ") // one line, never a header
	var footer []string
	if v := vars["cardname"]; v != "" {
		footer = append(footer, "Card: "+v)
	}
	if v := vars["description"]; v != "" {
		footer = append(footer, "Description: "+v)
	}
	if v := vars["cardlink"]; v != "" {
		footer = append(footer, "Link: "+v)
	}
	text := body
	if len(footer) > 0 {
		if body != "" {
			text += "\n\n"
		}
		text += "-- \n" + strings.Join(footer, "\n")
	}
	// "Details": the card's own fields (rule_mail_details.go).
	if act["includeCardDetails"] == true {
		details, err := s.ruleMailDetails(ctx, docString(a, "userId"), c.BoardID, c.ID)
		if err != nil {
			return err
		}
		if details != "" {
			text += "\n\n" + details
		}
	}
	// "Checklists / Comments": the card's, as its user may read them; a card
	// they no longer may sends no mail at all, as in WeKan.
	if act["includeChecklistsAndComments"] == true {
		discussion, err := s.ruleMailDiscussion(ctx, docString(a, "userId"), c.BoardID, c.ID)
		if err != nil {
			return err
		}
		if discussion != "" {
			text += "\n\n" + discussion
		}
	}
	// "Attachments": the card's own files, with their manifest in the text.
	var files []mailAttachment
	if act["includeAttachments"] == true {
		var manifest string
		var err error
		if files, manifest, err = s.ruleMailAttachments(ctx, docString(a, "userId"), c.BoardID, c.ID); err != nil {
			return err
		}
		if manifest != "" {
			text += "\n\n" + manifest
		}
	}
	return s.sendMail(ctx, "localhost", mailMessage{To: to, Subject: subject, Text: text, Attachments: files})
}

// cardLink is the card's own address when Weddy knows its ROOT_URL, as
// WeKan's Card.absoluteUrl.
func cardLink(b *Board, c *Card) string {
	root := strings.TrimRight(os.Getenv("ROOT_URL"), "/")
	if root == "" || b == nil || c == nil {
		return ""
	}
	return root + "/b/" + b.ID + "/" + b.Slug + "/" + c.ID
}
