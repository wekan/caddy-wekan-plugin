package web

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// settingsScalarFields are the fields of WeKan's Settings schema
// (models/settings.js) a site admin writes through /settings/update: its
// Boolean and String fields, an allowed-values list for those that have one.
// Generated from the schema; its object and array fields (the mail server,
// OAuth providers, feature states ...) are written by their own methods.
var settingsScalarFields = map[string][]string{
	"accessibilityPageEnabled":          {"bool"},
	"activityPubEnabled":                {"bool"},
	"alwaysShowCodeAsText":              {"bool"},
	"anonymizeExportUsers":              {"bool"},
	"anonymizeImportUsers":              {"bool"},
	"boardMembersFromSameOrgOnly":       {"bool"},
	"boardMembersFromSameOrgOrTeamOnly": {"bool"},
	"boardMembersFromSameTeamOnly":      {"bool"},
	"customAssetLinksEnabled":           {"bool"},
	"customHeadEnabled":                 {"bool"},
	"customManifestEnabled":             {"bool"},
	"disableActivities":                 {"bool"},
	"disableAllExport":                  {"bool"},
	"disableAllImport":                  {"bool"},
	"disableExportAvatars":              {"bool"},
	"disableForgotPassword":             {"bool"},
	"disableImportAvatars":              {"bool"},
	"disableNotifications":              {"bool"},
	"disableRegistration":               {"bool"},
	"disableWatch":                      {"bool"},
	"displayAuthenticationMethod":       {"bool"},
	"enablePermanentDelete":             {"bool"},
	"enableSyncCronEffects":             {"bool"},
	"featureApprovalRequired":           {"bool"},
	"featurePreviewAdmins":              {"bool"},
	"hideBoardActivitiesOnAllBoards":    {"bool"},
	"hideBoardMemberList":               {"bool"},
	"hideCardCounterList":               {"bool"},
	"hideDateFormat":                    {"bool"},
	"hideLogo":                          {"bool"},
	"notifyDefaultEmail":                {"bool"},
	"notifyDefaultTray":                 {"bool"},
	"oauthProvidersMergeExistingUsers":  {"bool"},
	"passwordlessEnabled":               {"bool"},
	"renderLinksAsPlainText":            {"bool"},
	"rulesReadOnlyForMembers":           {"bool"},
	"sqrlEnabled":                       {"bool"},
	"supportPageEnabled":                {"bool"},
	"supportPagePublic":                 {"bool"},
	"accessibilityContent":              {"string"},
	"accessibilityTitle":                {"string"},
	"activityEmailBodyTemplate":         {"string"},
	"activityEmailSubjectTemplate":      {"string"},
	"automaticLinkedUrlSchemes":         {"string"},
	"customAssetLinksContent":           {"string"},
	"customHeadLinkTags":                {"string"},
	"customHeadMetaTags":                {"string"},
	"customHelpLinkUrl":                 {"string"},
	"customLoginLogoImageUrl":           {"string"},
	"customLoginLogoLinkUrl":            {"string"},
	"customManifestContent":             {"string"},
	"customPrivateBoardDesc":            {"string"},
	"customPublicBoardDesc":             {"string"},
	"customTopLeftCornerLogoHeight":     {"string"},
	"customTopLeftCornerLogoImageUrl":   {"string"},
	"customTopLeftCornerLogoLinkUrl":    {"string"},
	"defaultAuthenticationMethod":       {"string"},
	"externalLinkIdentifierAliases":     {"string"},
	"externalLinkPatternPrefix":         {"string"},
	"externalLinkPatternUrl":            {"string"},
	"externalLinkRules":                 {"string"},
	"inviteEmailBodyTemplate":           {"string"},
	"inviteEmailSubjectTemplate":        {"string"},
	"legalNotice":                       {"string"},
	"loginBackgroundColor":              {"string"},
	"mailDomainName":                    {"string"},
	"oauthProvidersAllowedEmailDomains": {"string"},
	"oidcBtnText":                       {"string"},
	"productName":                       {"string"},
	"spinnerName":                       {"string"},
	"supportPageText":                   {"string"},
	"supportPopupText":                  {"string"},
	"supportTitle":                      {"string"},
	"textBelowCustomLoginLogo":          {"string"},
	"cardsLoading":                      {"string", "all", "lazy"},
	"globalDateFormat":                  {"string", "YYYY-MM-DD", "DD-MM-YYYY", "MM-DD-YYYY", "YYYY-MM-DD-date-only", "DD-MM-YYYY-date-only", "MM-DD-YYYY-date-only"},
	"oauthProvidersLoginStyle":          {"string", "popup", "redirect"},
}

// settingsRequiredFields may be set, never unset.
var settingsRequiredFields = map[string]bool{"defaultAuthenticationMethod": true}

// methodSettingsUpdate is the settings collection's own /settings/update as
// WeKan's allow rule lets it through (server/permissions/settings.js): a site
// admin's, of the schema's scalar fields, each checked as the schema checks
// it - a Boolean, a String, one of its allowed values. Anybody else, any
// other field and any other operator is Meteor's 403.
func (s *Server) methodSettingsUpdate(r *http.Request, u *User, args []json.RawMessage) (any, error) {
	denied := &methodError{"403", "Access denied"}
	if !u.IsAdmin || len(args) < 2 {
		return nil, denied
	}
	var id string
	if json.Unmarshal(args[0], &id) != nil {
		var sel map[string]any
		if json.Unmarshal(args[0], &sel) != nil || len(sel) != 1 {
			return nil, denied
		}
		id, _ = sel["_id"].(string)
	}
	var mod map[string]map[string]any
	if id == "" || json.Unmarshal(args[1], &mod) != nil || len(mod) == 0 {
		return nil, denied
	}
	set, unset := bson.M{}, bson.M{}
	for op, fields := range mod {
		if (op != "$set" && op != "$unset") || len(fields) == 0 {
			return nil, denied
		}
		for k, v := range fields {
			kind, ok := settingsScalarFields[k]
			if !ok {
				return nil, denied
			}
			if op == "$unset" {
				if settingsRequiredFields[k] {
					return nil, &methodError{"400", k + " is required"}
				}
				unset[k] = ""
				continue
			}
			switch kind[0] {
			case "bool":
				b, isBool := v.(bool)
				if !isBool {
					return nil, &methodError{"400", k + " must be a boolean"}
				}
				set[k] = b
			default:
				str, isString := v.(string)
				if !isString || len(str) > 1<<20 || (len(kind) > 1 && !slices.Contains(kind[1:], str)) {
					return nil, &methodError{"400", k + " is not an allowed value"}
				}
				set[k] = str
			}
		}
	}
	ctx := r.Context()
	if n, _ := s.db.Collection("settings").CountDocuments(ctx, bson.M{"_id": eq(id)}); n == 0 {
		return 0, nil
	}
	set["modifiedAt"] = time.Now().UTC()
	update := bson.M{"$set": set}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	if _, err := s.db.Collection("settings").UpdateOne(ctx, bson.M{"_id": eq(id)}, update); err != nil {
		return nil, err
	}
	return 1, nil
}
