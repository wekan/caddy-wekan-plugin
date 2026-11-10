---

kanban-plugin: board
cssclass: "wide"
42: number key
lane-width: 300

---

stray line before a lane
- [ ] Before any lane #early

## Doing (3)

- [ ] Title #tag #two @{15.10.2026} @@{14:30} ⏫ key:: value
    more body
    - [ ] step one
    - [x] step two
    Owner:: Ann
- [x] Done card 📅 2026-10-20 🛫 2026-10-01 ➕ 2026-09-01 ✅ 2026-10-21 🆔 abc ⛔ x1, y2
- [ ] Scheduled ⏳ 2026-11-01 ❌ 2026-11-02 🔁 every week 🔼🔽 ^blockid
- [ ] Bad date @{31.02.2026} first<br>second line<BR/>third
- [🔺] emoji box
- [ ] tags#not #a#b  spaced   out 
unindented note
    still body

## Doing (3)
- [ ] Same lane again

## Review (x)
**Complete**
- [ ] Completed by lane
- [ ] @[[01.01.2027]] wikilink date
##   
- [ ] Under an empty title

***

## Archive

- [x] Old card #old

%% kanban:settings
```
{"kanban-plugin":"board","date-format":"DD.MM.YYYY"}
```
%%
