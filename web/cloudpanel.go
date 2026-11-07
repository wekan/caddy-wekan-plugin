package web

// The Cloud Panel: many WeKan domains from one Weddy. It is Weddy's, not
// WeKan's - here every domain's WeKan is in one process, where Node.js WeKan
// containers would have to work together.
//
// The primary domain's WeKan (CloudDir set) has Admin Panel / People / Cloud,
// where a site admin names the Cloud Panel Domain and the SuperAdmin. At the
// Cloud Panel Domain - served by the same WeKan - the SuperAdmin gets the Cloud
// Panel: Settings with the left menus Domains and CloudFlare. Every request asks
// again: the SuperAdmin, at the Cloud Panel Domain. A customer's WeKan (Tenant)
// has none of it.
//
//	<cloud>/settings.json    {"superAdminUsername": "..."}
//	<cloud>/domains.json     {"cloudPanelDomain": "...", "domains": [...]}   read by Weddy
//	<cloud>/cloudflare.json  {"apiToken": "..."}  mode 0600, never sent to a browser
//	<cloud>/status.json      what Weddy did for each domain (the TLS column)

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var hostname = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,62}$`)

// ValidDomain reports whether name is a domain name that can also be a directory
// name: lower case letters, digits, hyphens and dots, no "..", no "/".
func ValidDomain(name string) bool { return len(name) <= 253 && hostname.MatchString(name) }

// NormalizeHost is a Host as a domain: lower case, no scheme, path, port or final dot.
func NormalizeHost(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	h, _, _ = strings.Cut(h, "/")
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.TrimRight(h, ".")
}

// Domain is one row of the Domains table.
type Domain struct {
	Domain        string `json:"domain"`
	CustomerName  string `json:"customerName,omitempty"`
	CustomerEmail string `json:"customerEmail,omitempty"`
	BillingEmail  string `json:"billingEmail,omitempty"`
	StartDate     string `json:"startDate,omitempty"` // YYYY-MM-DD, the first day of the paid hosting year
	EndDate       string `json:"endDate,omitempty"`   // its last day
}

// Active reports whether the domain is in its paid hosting period on day: from
// its start date to its end date, both included; a domain without dates is.
func (d Domain) Active(day time.Time) bool {
	today := day.UTC().Format("2006-01-02")
	return (d.StartDate == "" || d.StartDate <= today) && (d.EndDate == "" || today <= d.EndDate)
}

// DomainsFile is <cloud>/domains.json.
type DomainsFile struct {
	CloudPanelDomain string   `json:"cloudPanelDomain,omitempty"`
	Domains          []Domain `json:"domains"`
}

var email = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)

func validDate(s string) bool {
	t, err := time.Parse("2006-01-02", s)
	return err == nil && t.Format("2006-01-02") == s
}

// CleanDomain is a row as it is stored, or the name of the field that is wrong.
func CleanDomain(d Domain) (Domain, string) {
	d.Domain = NormalizeHost(d.Domain)
	for _, p := range []*string{&d.CustomerName, &d.CustomerEmail, &d.BillingEmail, &d.StartDate, &d.EndDate} {
		*p = strings.TrimSpace(*p)
	}
	switch {
	case !ValidDomain(d.Domain):
		return d, "domain"
	case d.CustomerEmail != "" && !email.MatchString(d.CustomerEmail):
		return d, "customerEmail"
	case d.BillingEmail != "" && !email.MatchString(d.BillingEmail):
		return d, "billingEmail"
	case d.StartDate != "" && !validDate(d.StartDate):
		return d, "startDate"
	case d.EndDate != "" && !validDate(d.EndDate):
		return d, "endDate"
	case d.StartDate != "" && d.EndDate != "" && d.EndDate < d.StartDate:
		return d, "endDate"
	case len(d.CustomerName) > 200:
		return d, "customerName"
	}
	return d, ""
}

// IsSuperAdmin: the user whose username, or one of whose e-mail addresses, is
// the SuperAdmin Username, without case. Nobody when it is empty.
func IsSuperAdmin(u *User, superAdmin string) bool {
	want := strings.ToLower(strings.TrimSpace(superAdmin))
	if want == "" || u == nil {
		return false
	}
	if strings.ToLower(u.Username) == want {
		return true
	}
	for _, e := range u.Emails {
		if strings.ToLower(strings.TrimSpace(e.Address)) == want {
			return true
		}
	}
	return false
}

type cloudSettings struct {
	SuperAdminUsername string `json:"superAdminUsername,omitempty"`
}

// cloudFiles reads and writes the files of <cloud>, one writer at a time.
type cloudFiles struct {
	dir string
	mu  sync.Mutex
}

func (c *cloudFiles) read(name string, v any) error {
	b, err := os.ReadFile(filepath.Join(c.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// write replaces a file whole - a temporary file renamed over it - so Weddy never
// reads half of one.
func (c *cloudFiles) write(name string, v any, mode os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.dir, "."+name+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(c.dir, name))
}

func (s *Server) cloudRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /admin/people/cloud", s.page(s.cloudSettingsPage))
	m.HandleFunc("POST /admin/people/cloud", s.page(s.saveCloudSettings))
	m.HandleFunc("GET /cloud", s.page(s.cloudPanel))
	m.HandleFunc("POST /cloud/domains", s.page(s.saveDomain))
	m.HandleFunc("POST /cloud/domains/delete", s.page(s.deleteDomain))
	m.HandleFunc("POST /cloud/cloudflare", s.page(s.saveCloudflare))
}

// cloudOn: the Cloud exists in this WeKan - the primary's, not a customer's.
func (s *Server) cloudOn() bool { return s.CloudDir != "" && !s.Tenant }

func (s *Server) cloud() *cloudFiles {
	s.cloudOnce.Do(func() { s.cloudF = &cloudFiles{dir: s.CloudDir} })
	return s.cloudF
}

// panelAllowed: the SuperAdmin, at the Cloud Panel Domain - asked on every request.
func (s *Server) panelAllowed(r *http.Request, u *User) bool {
	if !s.cloudOn() {
		return false
	}
	var set cloudSettings
	var file DomainsFile
	if s.cloud().read("settings.json", &set) != nil || s.cloud().read("domains.json", &file) != nil {
		return false
	}
	panel := NormalizeHost(file.CloudPanelDomain)
	return ValidDomain(panel) && NormalizeHost(r.Host) == panel && IsSuperAdmin(u, set.SuperAdminUsername)
}

func (s *Server) cloudSettingsPage(w http.ResponseWriter, r *http.Request, u *User) {
	if !s.cloudOn() || !u.IsAdmin {
		s.render(w, r, "notfound", http.StatusNotFound, map[string]any{"Title": tr(languageOf(r, u).Tag, "error-notAuthorized"), "User": u})
		return
	}
	var set cloudSettings
	var file DomainsFile
	_ = s.cloud().read("settings.json", &set)
	_ = s.cloud().read("domains.json", &file)
	s.render(w, r, "cloudsettings", http.StatusOK, map[string]any{"Title": tr(languageOf(r, u).Tag, "cloud"), "User": u,
		"PanelDomain": file.CloudPanelDomain, "SuperAdmin": set.SuperAdminUsername, "Saved": r.URL.Query().Has("saved")})
}

func (s *Server) saveCloudSettings(w http.ResponseWriter, r *http.Request, u *User) {
	if !s.cloudOn() || !u.IsAdmin {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	raw := strings.TrimSpace(r.FormValue("cloudPanelDomain"))
	panel := NormalizeHost(raw)
	if raw != "" && !ValidDomain(panel) { // "../evil" normalizes to "": refused, not cleared
		http.Error(w, "not a domain name", http.StatusBadRequest)
		return
	}
	superAdmin := strings.TrimSpace(r.FormValue("superAdminUsername"))
	if len(superAdmin) > 254 {
		http.Error(w, "too long", http.StatusBadRequest)
		return
	}
	c := s.cloud()
	c.mu.Lock()
	defer c.mu.Unlock()
	var file DomainsFile
	if err := c.read("domains.json", &file); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	file.CloudPanelDomain = panel
	if err := c.write("domains.json", sortedFile(file), 0o640); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := c.write("settings.json", cloudSettings{SuperAdminUsername: superAdmin}, 0o600); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/admin/people/cloud?saved", http.StatusSeeOther)
}

func sortedFile(f DomainsFile) DomainsFile {
	if f.Domains == nil {
		f.Domains = []Domain{}
	}
	sort.Slice(f.Domains, func(i, j int) bool { return f.Domains[i].Domain < f.Domains[j].Domain })
	return f
}

// domainStatus is a row of <cloud>/status.json, written by Weddy.
type domainStatus struct {
	Domain string `json:"domain"`
	TLS    string `json:"tls"`
	Error  string `json:"error,omitempty"`
}

// panelRow is a Domains table row with what Weddy did for it.
type panelRow struct {
	Domain
	TLS, TLSError string
}

func (s *Server) cloudPanel(w http.ResponseWriter, r *http.Request, u *User) {
	lang := languageOf(r, u).Tag
	if !s.panelAllowed(r, u) {
		s.render(w, r, "cloudpanel", http.StatusForbidden, map[string]any{"Title": tr(lang, "cloud"), "User": u, "NotAllowed": true})
		return
	}
	var file DomainsFile
	var status struct {
		Domains []domainStatus `json:"domains"`
	}
	var cf struct {
		APIToken string `json:"apiToken"`
	}
	_ = s.cloud().read("domains.json", &file)
	_ = s.cloud().read("status.json", &status)
	_ = s.cloud().read("cloudflare.json", &cf)
	tls := map[string]domainStatus{}
	for _, d := range status.Domains {
		tls[d.Domain] = d
	}
	rows := make([]panelRow, 0, len(file.Domains))
	for _, d := range sortedFile(file).Domains {
		rows = append(rows, panelRow{Domain: d, TLS: tls[d.Domain].TLS, TLSError: tls[d.Domain].Error})
	}
	pane := r.URL.Query().Get("pane")
	if pane != "cloudflare" {
		pane = "domains"
	}
	var edit Domain
	if e := NormalizeHost(r.URL.Query().Get("edit")); e != "" {
		for _, d := range file.Domains {
			if d.Domain == e {
				edit = d
			}
		}
	}
	s.render(w, r, "cloudpanel", http.StatusOK, map[string]any{"Title": tr(lang, "cloud"), "User": u, "Pane": pane,
		"Rows": rows, "Edit": edit, "TokenSet": cf.APIToken != "", "Saved": r.URL.Query().Has("saved"),
		"Error": r.URL.Query().Get("error")})
}

// refuse answers a Cloud Panel change that is not the SuperAdmin's at the panel.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, u *User) bool {
	if s.panelAllowed(r, u) {
		return false
	}
	http.Error(w, "The Cloud Panel is only for the SuperAdmin, at the Cloud Panel Domain.", http.StatusForbidden)
	return true
}

func (s *Server) saveDomain(w http.ResponseWriter, r *http.Request, u *User) {
	if s.refuse(w, r, u) {
		return
	}
	d, bad := CleanDomain(Domain{Domain: r.FormValue("domain"), CustomerName: r.FormValue("customerName"),
		CustomerEmail: r.FormValue("customerEmail"), BillingEmail: r.FormValue("billingEmail"),
		StartDate: r.FormValue("startDate"), EndDate: r.FormValue("endDate")})
	if bad != "" {
		http.Redirect(w, r, "/cloud?error="+bad, http.StatusSeeOther)
		return
	}
	c := s.cloud()
	c.mu.Lock()
	defer c.mu.Unlock()
	var file DomainsFile
	if err := c.read("domains.json", &file); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	old := NormalizeHost(r.FormValue("original"))
	kept := file.Domains[:0]
	for _, x := range file.Domains {
		if x.Domain != d.Domain && (old == "" || x.Domain != old) {
			kept = append(kept, x)
		}
	}
	file.Domains = append(kept, d)
	if err := c.write("domains.json", sortedFile(file), 0o640); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/cloud?saved", http.StatusSeeOther)
}

func (s *Server) deleteDomain(w http.ResponseWriter, r *http.Request, u *User) {
	if s.refuse(w, r, u) {
		return
	}
	name := NormalizeHost(r.FormValue("domain"))
	c := s.cloud()
	c.mu.Lock()
	defer c.mu.Unlock()
	var file DomainsFile
	if err := c.read("domains.json", &file); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	kept := file.Domains[:0]
	for _, x := range file.Domains {
		if x.Domain != name {
			kept = append(kept, x)
		}
	}
	file.Domains = kept
	if err := c.write("domains.json", sortedFile(file), 0o640); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/cloud?saved", http.StatusSeeOther)
}

// saveCloudflare keeps the API token on the server only; an empty field keeps
// the saved one, and "clear" removes it.
func (s *Server) saveCloudflare(w http.ResponseWriter, r *http.Request, u *User) {
	if s.refuse(w, r, u) {
		return
	}
	token := strings.TrimSpace(r.FormValue("apiToken"))
	c := s.cloud()
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case r.FormValue("clear") != "":
		if err := os.Remove(filepath.Join(c.dir, "cloudflare.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	case token != "":
		if len(token) > 512 || strings.ContainsAny(token, " \t\r\n") {
			http.Redirect(w, r, "/cloud?pane=cloudflare&error=apiToken", http.StatusSeeOther)
			return
		}
		if err := c.write("cloudflare.json", map[string]string{"apiToken": token}, 0o600); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	http.Redirect(w, r, "/cloud?pane=cloudflare&saved", http.StatusSeeOther)
}
