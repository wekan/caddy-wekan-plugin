# WeKan's Jade templates as Go Pug

WeKan's client has 174 Jade templates (`client/**/*.jade`). Each becomes a Go Pug
template in `web/templates/`, page by page, keeping WeKan's class names so WeKan's
Playwright page objects find the same elements. This table is the plan and the
progress; update it with the template.

| Status | Count |
|---|---|
| started - the first part of the page | 19 |
| not yet | 155 |

| WeKan template | Go Pug | Status |
|---|---|---|
| `components/activities/activities.jade` | board.pug: the card's and the board's activities, in WeKan's words | started |
| `components/activities/comments.jade` | board.pug: the card's comments and the comment form | started |
| `components/boardConversionProgress.jade` | | not yet |
| `components/boards/allBoardsSidebar.jade` | | not yet |
| `components/boards/bigboardView.jade` | | not yet |
| `components/boards/boardAnnouncement.jade` | | not yet |
| `components/boards/boardArchive.jade` | | not yet |
| `components/boards/boardBody.jade` | board.pug: the board canvas | started |
| `components/boards/boardHeader.jade` | | not yet |
| `components/boards/boardsList.jade` | boards.pug: the list of boards and Add Board | started |
| `components/boards/calendarModeView.jade` | | not yet |
| `components/boards/chartPlaceholderViews.jade` | | not yet |
| `components/boards/charts/boardCharts.jade` | | not yet |
| `components/boards/charts/exportChart.jade` | | not yet |
| `components/boards/chatView.jade` | | not yet |
| `components/boards/columnSetsView.jade` | | not yet |
| `components/boards/dependencyOverlay.jade` | | not yet |
| `components/boards/emailView.jade` | | not yet |
| `components/boards/exportScope.jade` | board.pug: the Export pop-over's formats (JSON, .zip, CSV, TSV, iCalendar) | started |
| `components/boards/groupByAssigneeView.jade` | | not yet |
| `components/boards/mapView.jade` | | not yet |
| `components/boards/miniboard.jade` | | not yet |
| `components/boards/multiboardCalendarView.jade` | | not yet |
| `components/boards/publicBoards.jade` | | not yet |
| `components/boards/roadmapView.jade` | | not yet |
| `components/boards/scrum/scrumDailyHistory.jade` | | not yet |
| `components/boards/scrum/scrumFields.jade` | | not yet |
| `components/boards/scrum/scrumScopeHistory.jade` | | not yet |
| `components/boards/scrum/scrumView.jade` | | not yet |
| `components/boards/statsView.jade` | | not yet |
| `components/boards/tableView.jade` | | not yet |
| `components/boards/timelineView.jade` | | not yet |
| `components/boards/timeView.jade` | | not yet |
| `components/boards/versionControlView.jade` | | not yet |
| `components/cards/attachments.jade` | board.pug: the card's attachment list, upload and delete | started |
| `components/cards/cardCustomFields.jade` | | not yet |
| `components/cards/cardDate.jade` | | not yet |
| `components/cards/cardDescription.jade` | | not yet |
| `components/cards/cardDetails.jade` | board.pug: title and description | started |
| `components/cards/cardFlowtime.jade` | | not yet |
| `components/cards/cardPomodoro.jade` | | not yet |
| `components/cards/cardStickers.jade` | | not yet |
| `components/cards/cardTextNotes.jade` | | not yet |
| `components/cards/cardTime.jade` | | not yet |
| `components/cards/checklists.jade` | | not yet |
| `components/cards/fromText.jade` | | not yet |
| `components/cards/inlinedCardDescription.jade` | | not yet |
| `components/cards/labels.jade` | | not yet |
| `components/cards/minicard.jade` | board.pug: the title | started |
| `components/cards/resultCard.jade` | | not yet |
| `components/cards/similarCards.jade` | | not yet |
| `components/cards/subtasks.jade` | | not yet |
| `components/cards/whiteboard.jade` | | not yet |
| `components/forms/calendarDateInput.jade` | | not yet |
| `components/forms/calendarTimeInput.jade` | | not yet |
| `components/forms/dateFormatSettings.jade` | | not yet |
| `components/forms/datepicker.jade` | | not yet |
| `components/forms/inlinedform.jade` | | not yet |
| `components/forms/multilineTitleChoice.jade` | | not yet |
| `components/forms/titleDueHint.jade` | | not yet |
| `components/gantt/dhtmlxGantt.jade` | | not yet |
| `components/gantt/frappeGantt.jade` | | not yet |
| `components/gantt/ganttCard.jade` | | not yet |
| `components/gantt/gantt.jade` | | not yet |
| `components/history/historyTable.jade` | | not yet |
| `components/import/csvMapping.jade` | | not yet |
| `components/import/import.jade` | | not yet |
| `components/import/trelloApi.jade` | | not yet |
| `components/lib/basicTabs.jade` | | not yet |
| `components/lists/listBody.jade` | board.pug: cards, the add-card form | started |
| `components/lists/listHeader.jade` | board.pug: title, + | started |
| `components/lists/list.jade` | | not yet |
| `components/lists/listPermissions.jade` | | not yet |
| `components/lists/minilist.jade` | | not yet |
| `components/main/accessibility.jade` | | not yet |
| `components/main/activityFeed.jade` | | not yet |
| `components/main/boardDragSettings.jade` | | not yet |
| `components/main/boardSettingsColumnActions.jade` | | not yet |
| `components/main/bookmarks.jade` | | not yet |
| `components/main/brokenCards.jade` | | not yet |
| `components/main/dueCards.jade` | | not yet |
| `components/main/editor.jade` | | not yet |
| `components/main/globalSearch.jade` | | not yet |
| `components/main/header.jade` | layout.pug: All Boards, user, Log Out | started |
| `components/main/historyRecoveryNotice.jade` | | not yet |
| `components/main/keyboardShortcuts.jade` | | not yet |
| `components/main/layouts.jade` | layout.pug, signin.pug, signup.pug, notfound.pug | started |
| `components/main/mapProvider.jade` | | not yet |
| `components/main/myAttachments.jade` | | not yet |
| `components/main/myCards.jade` | | not yet |
| `components/main/pageSidebar.jade` | | not yet |
| `components/main/popup.tpl.jade` | | not yet |
| `components/main/recoveryMaintenance.jade` | | not yet |
| `components/main/shareTarget.jade` | | not yet |
| `components/main/spinner_bounce.jade` | | not yet |
| `components/main/spinner_cube_grid.jade` | | not yet |
| `components/main/spinner_cube.jade` | | not yet |
| `components/main/spinner_dot.jade` | | not yet |
| `components/main/spinner_double_bounce.jade` | | not yet |
| `components/main/spinner.jade` | | not yet |
| `components/main/spinner_rotateplane.jade` | | not yet |
| `components/main/spinner_scaleout.jade` | | not yet |
| `components/main/spinner_wave.jade` | | not yet |
| `components/main/starredItems.jade` | | not yet |
| `components/main/structuralSelection.jade` | | not yet |
| `components/main/support.jade` | | not yet |
| `components/main/themeColorPicker.jade` | | not yet |
| `components/notifications/notificationIcon.jade` | | not yet |
| `components/notifications/notification.jade` | | not yet |
| `components/notifications/notificationsDrawer.jade` | | not yet |
| `components/notifications/notifications.jade` | | not yet |
| `components/rules/actions/boardActions.jade` | | not yet |
| `components/rules/actions/cardActions.jade` | | not yet |
| `components/rules/actions/checklistActions.jade` | | not yet |
| `components/rules/actions/mailActions.jade` | | not yet |
| `components/rules/boardButtons.jade` | | not yet |
| `components/rules/cardButtons.jade` | | not yet |
| `components/rules/ruleDetails.jade` | | not yet |
| `components/rules/rulesActions.jade` | | not yet |
| `components/rules/rulesBlocks.jade` | | not yet |
| `components/rules/rulesBots.jade` | | not yet |
| `components/rules/rulesImportExport.jade` | | not yet |
| `components/rules/rulesList.jade` | | not yet |
| `components/rules/rulesMain.jade` | | not yet |
| `components/rules/rulesTriggers.jade` | | not yet |
| `components/rules/ruleSuggestions.jade` | | not yet |
| `components/rules/rulesWorkflow.jade` | | not yet |
| `components/rules/ruleVariablePicker.jade` | | not yet |
| `components/rules/triggers/boardTriggers.jade` | | not yet |
| `components/rules/triggers/buttonTriggers.jade` | | not yet |
| `components/rules/triggers/cardTriggers.jade` | | not yet |
| `components/rules/triggers/checklistTriggers.jade` | | not yet |
| `components/rules/triggers/scheduledTriggers.jade` | | not yet |
| `components/settings/adminAuditLog.jade` | | not yet |
| `components/settings/adminProblems.jade` | problems.pug: Recovery's e-mail delivery queue and pending activity notifications - paused, resumed, cancelled, retried | started |
| `components/settings/attachments.jade` | | not yet |
| `components/settings/authProviderSettings.jade` | | not yet |
| `components/settings/backupRunsStatus.jade` | | not yet |
| `components/settings/cloudPanel.jade` | | not yet |
| `components/settings/connectionMethod.jade` | | not yet |
| `components/settings/emailBounces.jade` | | not yet |
| `components/settings/emailToCardSettings.jade` | | not yet |
| `components/settings/informationBody.jade` | | not yet |
| `components/settings/invitationCode.jade` | | not yet |
| `components/settings/leftMenu.jade` | | not yet |
| `components/settings/migrationProgress.jade` | | not yet |
| `components/settings/notificationDeliverySettings.jade` | | not yet |
| `components/settings/notificationSettingsPopup.jade` | | not yet |
| `components/settings/passwordLengthReport.jade` | | not yet |
| `components/settings/peopleBody.jade` | people.pug: users, admin and active, registration; cloudsettings.pug: People / Cloud | started |
| `components/settings/problemsSummary.jade` | | not yet |
| `components/settings/settingBody.jade` | cloudpanel.pug: the Cloud Panel's Settings, Domains and CloudFlare | started |
| `components/settings/settingHeader.jade` | | not yet |
| `components/settings/sharedLabelsSettings.jade` | | not yet |
| `components/settings/tablePage.jade` | | not yet |
| `components/settings/translationBody.jade` | | not yet |
| `components/sidebar/sidebarArchives.jade` | | not yet |
| `components/sidebar/sidebarCustomFields.jade` | | not yet |
| `components/sidebar/sidebarFilters.jade` | | not yet |
| `components/sidebar/sidebar.jade` | board.pug: the sidebar, the board menu with Export | started |
| `components/sidebar/sidebarOriginalPositions.jade` | | not yet |
| `components/sidebar/sidebarSearches.jade` | | not yet |
| `components/sidebar/sidebarSinceLastVisit.jade` | | not yet |
| `components/sidebar/sidebarSummary.jade` | | not yet |
| `components/swimlanes/miniswimlane.jade` | | not yet |
| `components/swimlanes/swimlaneHeader.jade` | board.pug: the title | started |
| `components/swimlanes/swimlanes.jade` | board.pug: swimlanes and their lists, Add List | started |
| `components/users/dependencyMenu.jade` | | not yet |
| `components/users/passwordChangeRequired.jade` | | not yet |
| `components/users/passwordInput.jade` | | not yet |
| `components/users/twoFactorRequired.jade` | | not yet |
| `components/users/userAvatar.jade` | | not yet |
| `components/users/userHeader.jade` | profile.pug: Edit Profile, Change Password; language.pug: Change Language | started |
| `lib/titleViewer.jade` | | not yet |
