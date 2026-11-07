package caddywekan

// Many domains from one Weddy: every customer domain is a WeKan of its own, in a
// directory of its own - <root>/domains/<domain>/files/db/ for its database,
// .../files/attachments and so on - started from the same bundle, and reached only
// through its own domain.
//
// The first domain, primary, is the WeKan whose Admin Panel / People / Cloud names
// the Cloud Panel Domain and the SuperAdmin; on the Cloud Panel Domain that WeKan
// shows the Cloud Panel, where the SuperAdmin keeps the table of customer domains
// and the CloudFlare API key. That WeKan writes them to <root>/cloud/, server side,
// and this file reads them:
//
//	<root>/cloud/domains.json      {"cloudPanelDomain": "...", "domains": [{"domain": ..., "startDate": ..., "endDate": ...}, ...]}
//	<root>/cloud/cloudflare.json   {"apiToken": "..."}        (mode 0600; never sent to a browser)
//	<root>/cloud/certs/<domain>.crt, .key   CloudFlare Origin certificates (cloudflare.go)
//	<root>/cloud/status.json       what this side did, for the Cloud Panel to show
//
// A domain is served from its startDate to its endDate, both included; a domain
// without dates is served. Changes are picked up within seconds, without a restart.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wekan/caddy-gitea-plugin/supervisor"
)

// Domain is one row of the Cloud Panel's Domains table.
type Domain struct {
	Domain        string `json:"domain"`
	CustomerName  string `json:"customerName,omitempty"`
	CustomerEmail string `json:"customerEmail,omitempty"`
	BillingEmail  string `json:"billingEmail,omitempty"`
	StartDate     string `json:"startDate,omitempty"` // YYYY-MM-DD
	EndDate       string `json:"endDate,omitempty"`   // YYYY-MM-DD, the last day of the paid hosting
}

// DomainsFile is <root>/cloud/domains.json.
type DomainsFile struct {
	CloudPanelDomain string   `json:"cloudPanelDomain,omitempty"`
	Domains          []Domain `json:"domains"`
}

var hostname = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,62}$`)

// ValidDomain reports whether name is a domain name that can also be a directory
// name: lower case letters, digits, hyphens and dots, no "..", no "/".
func ValidDomain(name string) bool { return len(name) <= 253 && hostname.MatchString(name) }

// NormalizeHost is a request's Host as a domain: lower case, no port, no final dot.
func NormalizeHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

// Active reports whether the domain is in its paid hosting period on day.
func (d Domain) Active(day time.Time) bool {
	today := day.UTC().Format("2006-01-02")
	return (d.StartDate == "" || d.StartDate <= today) && (d.EndDate == "" || today <= d.EndDate)
}

type instance struct {
	domain string
	proc   *supervisor.Process
	proxy  *httputil.ReverseProxy
}

// Cloud runs the WeKan of every domain.
type Cloud struct {
	app *App
	log *zap.Logger

	mu        sync.RWMutex
	instances map[string]*instance // by domain
	panel     string               // the Cloud Panel Domain: served by the primary's WeKan
	active    map[string]bool      // domains with a certificate allowed
	modTime   time.Time
	stop      chan struct{}
	done      chan struct{}
}

var current struct {
	sync.RWMutex
	cloud *Cloud
}

// CurrentCloud is the running Cloud, for the TLS modules; nil without one.
func CurrentCloud() *Cloud {
	current.RLock()
	defer current.RUnlock()
	return current.cloud
}

func (a *App) cloudDir() string { return filepath.Join(a.Root, "cloud") }

func newCloud(a *App, log *zap.Logger) *Cloud {
	return &Cloud{app: a, log: log, instances: map[string]*instance{}, active: map[string]bool{}}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// start runs the WeKan of one domain from <root>/domains/<domain>.
func (c *Cloud) start(domain string, primary bool) (*instance, error) {
	if !ValidDomain(domain) {
		return nil, fmt.Errorf("not a domain name: %q", domain)
	}
	dir := filepath.Join(c.app.Root, "domains", domain)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	web, err := freePort()
	if err != nil {
		return nil, err
	}
	db, err := freePort()
	if err != nil {
		return nil, err
	}
	vals := map[string]string{}
	for k, v := range c.app.Env {
		vals[k] = v
	}
	vals["ROOT_URL"] = "https://" + domain
	vals["BIND_IP"], vals["PORT"] = "127.0.0.1", fmt.Sprint(web)
	vals["WRITABLE_PATH"] = dir // the launcher keeps files/ - and files/db/ - below it
	vals["FERRETDB_LISTEN_ADDR"] = fmt.Sprintf("127.0.0.1:%d", db)
	delete(vals, "MONGO_URL") // each domain its own database; the launcher derives it
	delete(vals, "UNIX_SOCKET_PATH")
	if primary {
		vals["WEDDY_CLOUD_DIR"] = c.app.cloudDir() // this WeKan has Admin Panel / People / Cloud
		delete(vals, "WEDDY_TENANT")
	} else {
		vals["WEDDY_TENANT"] = "true" // a customer's WeKan: no Cloud menu, no SuperAdmin
		delete(vals, "WEDDY_CLOUD_DIR")
	}
	env := make([]string, 0, len(vals))
	for k, v := range vals {
		env = append(env, k+"="+v)
	}
	sort.Strings(env)
	dial := fmt.Sprintf("127.0.0.1:%d", web)
	proc := supervisor.New(supervisor.Config{
		Name: "wekan " + domain, Command: c.app.Command, Args: c.app.Args, Dir: c.app.Bundle,
		Env: env, Dial: dial, StopTimeout: time.Duration(c.app.StopTimeout),
	}, c.log)
	if err := proc.Start(); err != nil {
		return nil, err
	}
	target := &url.URL{Scheme: "http", Host: dial}
	rp := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.Out.Host = r.In.Host // WeKan sees the domain it was asked for
		r.SetXForwarded()
	}}
	return &instance{domain: domain, proc: proc, proxy: rp}, nil
}

// Load reads domains.json and starts and stops WeKans to match it.
func (c *Cloud) Load(now time.Time) error {
	path := filepath.Join(c.app.cloudDir(), "domains.json")
	var file DomainsFile
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &file); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	want := map[string]bool{c.app.Primary: true}
	active := map[string]bool{c.app.Primary: true}
	panel := NormalizeHost(file.CloudPanelDomain)
	if panel != "" && ValidDomain(panel) {
		active[panel] = true
	} else {
		panel = ""
	}
	for _, d := range file.Domains {
		name := NormalizeHost(d.Domain)
		if !ValidDomain(name) {
			c.log.Warn("domains.json: not a domain name, skipped", zap.String("domain", d.Domain))
			continue
		}
		if d.Active(now) {
			want[name], active[name] = true, true
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.panel, c.active = panel, active
	for name, in := range c.instances {
		if !want[name] {
			c.log.Info("domain no longer served: stopping its WeKan", zap.String("domain", name))
			delete(c.instances, name)
			go in.proc.Stop()
		}
	}
	for name := range want {
		if _, ok := c.instances[name]; ok {
			continue
		}
		in, err := c.start(name, name == c.app.Primary)
		if err != nil {
			c.log.Error("starting the WeKan of a domain", zap.String("domain", name), zap.Error(err))
			continue
		}
		c.instances[name] = in
		c.log.Info("serving domain", zap.String("domain", name))
	}
	return nil
}

func (c *Cloud) watch() {
	defer close(c.done)
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	day := time.Now().UTC().Format("2006-01-02")
	for {
		select {
		case <-c.stop:
			return
		case <-tick.C:
		}
		path := filepath.Join(c.app.cloudDir(), "domains.json")
		st, err := os.Stat(path)
		today := time.Now().UTC().Format("2006-01-02")
		if (err == nil && !st.ModTime().Equal(c.modTime)) || today != day {
			if err == nil {
				c.modTime = st.ModTime()
			}
			day = today
			if err := c.Load(time.Now()); err != nil {
				c.log.Error("reading domains", zap.Error(err))
			}
			c.syncCloudflare()
		}
	}
}

// Start starts the primary's WeKan and every active domain's, and follows changes.
func (c *Cloud) Start() error {
	if err := os.MkdirAll(filepath.Join(c.app.cloudDir(), "certs"), 0o700); err != nil {
		return err
	}
	if st, err := os.Stat(filepath.Join(c.app.cloudDir(), "domains.json")); err == nil {
		c.modTime = st.ModTime()
	}
	if err := c.Load(time.Now()); err != nil {
		return err
	}
	current.Lock()
	current.cloud = c
	current.Unlock()
	c.stop, c.done = make(chan struct{}), make(chan struct{})
	go c.watch()
	go c.syncCloudflare()
	return nil
}

// Stop stops every domain's WeKan.
func (c *Cloud) Stop() error {
	if c.stop != nil {
		close(c.stop)
		<-c.done
	}
	current.Lock()
	if current.cloud == c {
		current.cloud = nil
	}
	current.Unlock()
	c.mu.Lock()
	all := c.instances
	c.instances = map[string]*instance{}
	c.mu.Unlock()
	var wg sync.WaitGroup
	for _, in := range all {
		wg.Add(1)
		go func(in *instance) { defer wg.Done(); _ = in.proc.Stop() }(in)
	}
	wg.Wait()
	return nil
}

// Allowed reports whether name may have a certificate: an active domain, the
// primary or the Cloud Panel Domain.
func (c *Cloud) Allowed(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.active[NormalizeHost(name)]
}

// Ready reports whether the WeKan of host accepts connections.
func (c *Cloud) Ready(host string) bool {
	in := c.lookup(host)
	return in != nil && in.proc.Ready()
}

func (c *Cloud) lookup(host string) *instance {
	name := NormalizeHost(host)
	c.mu.RLock()
	defer c.mu.RUnlock()
	if name != "" && name == c.panel {
		name = c.app.Primary
	}
	return c.instances[name]
}

// ServeHTTP sends the request to the WeKan of its domain - and only there: an
// unknown or expired domain gets 404, never another domain's WeKan.
func (c *Cloud) ServeHTTP(w http.ResponseWriter, r *http.Request) (handled bool, status int) {
	in := c.lookup(r.Host)
	if in == nil {
		return false, http.StatusNotFound
	}
	if !in.proc.Ready() {
		w.Header().Set("Retry-After", "5")
		return false, http.StatusServiceUnavailable
	}
	in.proxy.ServeHTTP(w, r)
	return true, 0
}
