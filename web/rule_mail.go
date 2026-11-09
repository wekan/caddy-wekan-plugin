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
	userID := docString(a, "userId")
	// A linked card's mail is about the card it shows (rule_mail_source.go):
	// its variables, its people, what is included.
	src, chain := c, []string{c.ID}
	linkedBoard := ""
	if c.Type == "cardType-linkedCard" || c.Type == "cardType-linkedBoard" {
		var err error
		if src, chain, err = s.ruleMailSource(ctx, userID, c); err != nil {
			return err
		}
		// A linked board shows that board's title and description
		// (rule_mail_linked_board.go).
		if src.Type == "cardType-linkedBoard" {
			if src, linkedBoard, err = s.ruleMailLinkedBoardView(ctx, userID, src); err != nil {
				return err
			}
		}
		placed := bson.M{}
		for k, v := range a {
			placed[k] = v
		}
		placed["boardId"], placed["cardId"] = src.BoardID, src.ID
		var actor *User
		var au User
		if validID(userID) && s.db.Collection("users").FindOne(ctx, bson.M{"_id": eq(userID)}).Decode(&au) == nil {
			actor = &au
		}
		vars = s.ruleVars(ctx, placed, src, actor)
	}
	// The people tokens are addresses here.
	people := map[string]string{}
	for token, ids := range map[string][]string{"creator": {src.UserID}, "assignees": src.Assignees, "members": src.Members} {
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
		details, err := s.ruleMailDetails(ctx, userID, src.BoardID, src.ID)
		if err != nil {
			return err
		}
		if details != "" {
			text += "\n\n" + details
		}
		// And the triggering card's own local details: a linked board's, or
		// a link's.
		switch {
		case c.Type == "cardType-linkedBoard":
			local, err := s.ruleMailDetailsOf(ctx, userID, c.BoardID, c.ID, true)
			if err != nil {
				return err
			}
			text += "\n\nLinked card local details:\n" + local
		case c.Type == "cardType-linkedCard":
			wrapper, err := s.ruleMailWrapper(ctx, userID, c)
			if err != nil {
				return err
			}
			text += "\n\n" + wrapper
		}
	}
	// "Checklists / Comments": the card's, as its user may read them; a card
	// they no longer may sends no mail at all, as in WeKan.
	if act["includeChecklistsAndComments"] == true {
		discussion, err := s.ruleMailDiscussion(ctx, userID, src.BoardID, src.ID)
		if err != nil {
			return err
		}
		if discussion != "" {
			text += "\n\n" + discussion
		}
		if linkedBoard != "" {
			boardTalk, err := s.ruleMailLinkedBoardDiscussion(ctx, userID, src.ID, linkedBoard)
			if err != nil {
				return err
			}
			if boardTalk != "" {
				text += "\n\n" + boardTalk
			}
		}
	}
	// "Attachments": the card's own files, with their manifest in the text.
	var files []mailAttachment
	if act["includeAttachments"] == true {
		var manifest string
		var err error
		if files, manifest, err = s.ruleMailAttachments(ctx, userID, src.BoardID, src.ID); err != nil {
			return err
		}
		if manifest != "" {
			text += "\n\n" + manifest
		}
	}
	// Every card on the way to the source is still readable and linked as it was.
	if err := s.ruleMailSourceCurrent(ctx, userID, chain); err != nil {
		return err
	}
	if linkedBoard != "" && !s.ruleMailBoardReadable(ctx, userID, linkedBoard) {
		return errRuleMailSource
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
