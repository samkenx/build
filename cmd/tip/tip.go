// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

// Command tip is the tip.golang.org server,
// serving the latest HEAD straight from the Git oven.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

const (
	repoURL      = "https://go.googlesource.com/"
	metaURL      = "https://go.googlesource.com/?b=master&format=JSON"
	startTimeout = 10 * time.Minute
)

var startTime = time.Now()

var (
	autoCertDomain      = flag.String("autocert", "", "if non-empty, listen on port 443 and serve a LetsEncrypt cert for this hostname or hostnames (comma-separated)")
	autoCertCacheBucket = flag.String("autocert-bucket", "", "if non-empty, the Google Cloud Storage bucket in which to store the LetsEncrypt cache")
)

func main() {
	flag.Parse()

	const k = "TIP_BUILDER"
	var b Builder
	switch os.Getenv(k) {
	case "golangorg":
		b = golangorgBuilder{}
	case "talks":
		b = talksBuilder{}
	default:
		log.Fatalf("Unknown %v value: %q", k, os.Getenv(k))
	}

	certInit()

	p := &Proxy{builder: b}
	go p.run()
	mux := newServeMux(p, serveOptions{
		// Redirect to HTTPS only if we're actually serving HTTPS.
		RedirectToHTTPS: *autoCertDomain != "",
	})

	log.Printf("Starting up tip server for builder %q", os.Getenv(k))

	errc := make(chan error, 1)
	go func() {
		errc <- http.ListenAndServe(":8080", wrapHTTPMux(mux))
	}()
	if *autoCertDomain != "" {
		go func() { errc <- runHTTPS(mux) }()
		log.Printf("Listening on port 443 with LetsEncrypt support on domain %q", *autoCertDomain)
	}
	if err := <-errc; err != nil {
		p.stop()
		log.Fatal(err)
	}
}

// Proxy implements the tip.golang.org server: a reverse-proxy
// that builds and runs golangorg instances showing the latest
// Go website and standard library documentation.
type Proxy struct {
	builder Builder

	mu       sync.Mutex // protects following fields
	proxy    http.Handler
	cur      string    // signature of gorepo+websiterepo
	cmd      *exec.Cmd // live golangorg instance, or nil for none
	side     string
	hostport string // host and port of the live instance
	err      error  // non-nil when there's a problem
}

type Builder interface {
	Signature(heads map[string]string) string
	Init(logger *log.Logger, dir, hostport string, heads map[string]string) (*exec.Cmd, error)
	HealthCheck(hostport string) error
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_tipstatus" {
		p.serveStatus(w, r)
		return
	}
	// Redirect the old beta.golang.org URL to tip.golang.org,
	// just in case there are old links out there to
	// beta.golang.org. (We used to run a "temporary" beta.golang.org
	// GCE VM running golangorg where "temporary" lasted two years.
	// So it lasted so long, there are probably links to it out there.)
	if r.Host == "beta.golang.org" {
		u := *r.URL
		u.Scheme = "https"
		u.Host = "tip.golang.org"
		http.Redirect(w, r, u.String(), http.StatusFound)
		return
	}
	p.mu.Lock()
	proxy := p.proxy
	err := p.err
	p.mu.Unlock()
	if proxy == nil {
		s := "starting up"
		if err != nil {
			s = err.Error()
		}
		http.Error(w, s, http.StatusInternalServerError)
		return
	}
	proxy.ServeHTTP(w, r)
}

func (p *Proxy) serveStatus(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(w, "side=%v\ncurrent=%v\nerror=%v\nuptime=%v\n", p.side, p.cur, p.err, int(time.Since(startTime).Seconds()))
}

func (p *Proxy) serveHealthCheck(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// NOTE: (App Engine only; not GKE) Status 502, 503, 504 are
	// the only status codes that signify an unhealthy app.  So
	// long as this handler returns one of those codes, this
	// instance will not be sent any requests.
	if p.proxy == nil {
		log.Printf("Health check: not ready")
		http.Error(w, "Not ready", http.StatusServiceUnavailable)
		return
	}

	if err := p.builder.HealthCheck(p.hostport); err != nil {
		log.Printf("Health check failed: %v", err)
		http.Error(w, "Health check failed", http.StatusServiceUnavailable)
		return
	}
	io.WriteString(w, "ok")
}

// run runs in its own goroutine.
func (p *Proxy) run() {
	p.mu.Lock()
	p.side = "a"
	p.mu.Unlock()
	for {
		p.poll()
		time.Sleep(30 * time.Second)
	}
}

func (p *Proxy) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil {
		p.cmd.Process.Kill()
	}
}

// poll runs from the run loop goroutine.
func (p *Proxy) poll() {
	heads := gerritMetaMap()
	if heads == nil {
		return
	}

	sig := p.builder.Signature(heads)

	p.mu.Lock()
	changes := sig != p.cur
	curSide := p.side
	p.cur = sig
	p.mu.Unlock()

	if !changes {
		return
	}

	newSide := "b"
	if curSide == "b" {
		newSide = "a"
	}

	dir := filepath.Join(os.TempDir(), "tip", newSide)
	if err := os.MkdirAll(dir, 0755); err != nil {
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		return
	}
	hostport := "localhost:8081"
	if newSide == "b" {
		hostport = "localhost:8082"
	}
	logger := log.New(os.Stderr, sig+": ", log.LstdFlags)

	cmd, err := p.builder.Init(logger, dir, hostport, heads)
	if err != nil {
		logger.Printf("Init failed: %v", err)
		err = fmt.Errorf("builder.Init: %v", err)
	} else {
		go func() {
			// TODO(adg,bradfitz): be smarter about dead processes
			if err := cmd.Wait(); err != nil {
				logger.Printf("process in %v exited: %v (%T)", dir, err, err)
				if ee, ok := err.(*exec.ExitError); ok {
					logger.Printf("ProcessState.Sys() = %v", ee.ProcessState.Sys())
				}
			}
		}()
		err = waitReady(p.builder, hostport)
		if err != nil {
			cmd.Process.Kill()
			err = fmt.Errorf("waitReady: %v", err)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		log.Println(err)
		p.err = err
		return
	}

	u, err := url.Parse(fmt.Sprintf("http://%v/", hostport))
	if err != nil {
		err = fmt.Errorf("parsing hostport: %v", err)
		log.Println(err)
		p.err = err
		return
	}
	p.proxy = httputil.NewSingleHostReverseProxy(u)
	p.side = newSide
	p.hostport = hostport
	if p.cmd != nil {
		p.cmd.Process.Kill()
	}
	p.cmd = cmd
	p.err = nil // If we get this far, the process started successfully. Clear the error.
	logger.Printf("success; starting to serve on side %v", newSide)
}

type serveOptions struct {
	// RedirectToHTTPS controls whether requests served
	// over HTTP should be redirected to HTTPS.
	RedirectToHTTPS bool
}

func newServeMux(p *Proxy, opt serveOptions) http.Handler {
	mux := http.NewServeMux()
	if opt.RedirectToHTTPS {
		mux.Handle("/", httpsOnlyHandler{p})
	} else {
		mux.Handle("/", p)
	}
	mux.HandleFunc("/_ah/health", p.serveHealthCheck)
	return mux
}

func waitReady(b Builder, hostport string) error {
	var err error
	deadline := time.Now().Add(startTimeout)
	for time.Now().Before(deadline) {
		if err = b.HealthCheck(hostport); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("timed out waiting for process at %v: %v", hostport, err)
}

func runErr(cmd *exec.Cmd) error {
	out, err := cmd.CombinedOutput()
	if err != nil {
		if len(out) == 0 {
			return err
		}
		return fmt.Errorf("%s\n%v", out, err)
	}
	return nil
}

func checkout(repo, hash, path string) error {
	// Clone git repo if it doesn't exist.
	if _, err := os.Stat(filepath.Join(path, ".git")); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return fmt.Errorf("mkdir: %v", err)
		}
		if err := runErr(exec.Command("git", "clone", "--depth", "1", "--", repo, path)); err != nil {
			return fmt.Errorf("clone: %v", err)
		}
	} else if err != nil {
		return fmt.Errorf("stat .git: %v", err)
	}

	// Pull down changes and update to hash.
	cmd := exec.Command("git", "fetch")
	cmd.Dir = path
	if err := runErr(cmd); err != nil {
		return fmt.Errorf("fetch: %v", err)
	}
	cmd = exec.Command("git", "reset", "--hard", hash, "--")
	cmd.Dir = path
	if err := runErr(cmd); err != nil {
		return fmt.Errorf("reset: %v", err)
	}
	cmd = exec.Command("git", "clean", "-d", "-f", "-x")
	cmd.Dir = path
	if err := runErr(cmd); err != nil {
		return fmt.Errorf("clean: %v", err)
	}
	return nil
}

var timeoutClient = &http.Client{Timeout: 10 * time.Second}

// gerritMetaMap returns the map from repo name (e.g. "go") to its
// latest master hash.
// The returned map is nil on any transient error.
func gerritMetaMap() map[string]string {
	// TODO(dmitshur): Replace with a Gerrit client implementation like in gitmirror.

	res, err := timeoutClient.Get(metaURL)
	if err != nil {
		log.Printf("Error getting Gerrit meta map: %v", err)
		return nil
	}
	defer res.Body.Close()
	defer io.Copy(ioutil.Discard, res.Body) // ensure EOF for keep-alive
	if res.StatusCode != 200 {
		return nil
	}
	var meta map[string]struct {
		Branches map[string]string
	}
	br := bufio.NewReader(res.Body)
	// For security reasons or something, this URL starts with ")]}'\n" before
	// the JSON object. So ignore that.
	// Shawn Pearce says it's guaranteed to always be just one line, ending in '\n'.
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil
		}
		if b == '\n' {
			break
		}
	}
	if err := json.NewDecoder(br).Decode(&meta); err != nil {
		log.Printf("JSON decoding error from %v: %s", metaURL, err)
		return nil
	}
	m := map[string]string{}
	for repo, v := range meta {
		if master, ok := v.Branches["master"]; ok {
			m[repo] = master
		}
	}
	return m
}

func getOK(url string) (body []byte, err error) {
	res, err := timeoutClient.Get(url)
	if err != nil {
		return nil, err
	}
	body, err = ioutil.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, errors.New(res.Status)
	}
	return body, nil
}

// httpsOnlyHandler redirects requests to "http://example.com/foo?bar" to
// "https://example.com/foo?bar". It should be used when the server is listening
// for HTTP traffic behind a proxy that terminates TLS traffic, not when the Go
// server is terminating TLS directly.
type httpsOnlyHandler struct {
	h http.Handler
}

// isProxiedReq checks whether the server is running behind a proxy that may be
// terminating TLS.
func isProxiedReq(r *http.Request) bool {
	if _, ok := r.Header["X-Appengine-Https"]; ok {
		return true
	}
	if _, ok := r.Header["X-Forwarded-Proto"]; ok {
		return true
	}
	return false
}

func (h httpsOnlyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Appengine-Https") == "off" || r.Header.Get("X-Forwarded-Proto") == "http" ||
		(!isProxiedReq(r) && r.TLS == nil) {
		r.URL.Scheme = "https"
		r.URL.Host = r.Host
		http.Redirect(w, r, r.URL.String(), http.StatusFound)
		return
	}
	if r.Header.Get("X-Appengine-Https") == "on" || r.Header.Get("X-Forwarded-Proto") == "https" ||
		(!isProxiedReq(r) && r.TLS != nil) {
		// Only set this header when we're actually in production.
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
	}
	h.h.ServeHTTP(w, r)
}

type toLoggerWriter struct{ logger *log.Logger }

func (w toLoggerWriter) Write(p []byte) (int, error) {
	w.logger.Printf("%s", p)
	return len(p), nil
}
