// Package cdp takes over Chromium-family browsers (Chrome, Edge, Chromium,
// Brave, BitBrowser windows) on a probe agent's machine through the Chrome
// DevTools Protocol. HTTP and WebSocket both ride the agent's TCP tunnel; the
// plugin adds browser discovery and launch, URL rewriting so DevTools
// clients connect through the hub, and an online CDP console.
package cdp

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Necho-dev/vartio/sdk"
	"io"
	"io/fs"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	Name        = "cdp"
	probeLimit  = 6 // concurrent /json/version probes; leaves tunnels for the user
	maxProbe    = 48
	launchWait  = 20 * time.Second
	maxJSONBody = 16 << 20
)

//go:embed ui
var uiFS embed.FS

type Plugin struct {
	host    sdk.Host
	tickets sdk.Tickets[wsTicket]
}

type wsTicket struct {
	Agent string
	Port  int
	Path  string
}

func New() *Plugin { return &Plugin{} }

func (p *Plugin) Init(_ context.Context, host sdk.Host) error {
	p.host = host
	return nil
}

func (p *Plugin) UI() fs.FS {
	sub, _ := fs.Sub(uiFS, "ui")
	return sub
}

func (p *Plugin) Routes(mux *http.ServeMux, prefix string) {
	mux.HandleFunc("GET "+prefix+"/catalog", func(w http.ResponseWriter, _ *http.Request) {
		sdk.WriteJSON(w, http.StatusOK, catalog)
	})
	mux.HandleFunc("GET "+prefix+"/agents/{id}/browsers", p.handleBrowsers)
	mux.HandleFunc("POST "+prefix+"/agents/{id}/launch", p.handleLaunch)
	mux.HandleFunc("POST "+prefix+"/agents/{id}/ports/{port}/ticket", p.handleTicket)
	mux.HandleFunc("POST "+prefix+"/agents/{id}/ports/{port}/close", p.handleClose)
	for _, m := range []string{"GET", "PUT", "POST"} {
		mux.HandleFunc(m+" "+prefix+"/agents/{id}/ports/{port}/json/{rest...}", p.handleJSON)
		mux.HandleFunc(m+" "+prefix+"/agents/{id}/ports/{port}/json", p.handleJSON)
	}
	mux.HandleFunc("GET "+prefix+"/agents/{id}/ports/{port}/devtools/{rest...}", p.handleDevtools)
	mux.HandleFunc("GET "+sdk.PublicPrefix+"/"+Name+"/{ticket}", p.handleTicketWS)
}

// ---------- catalog ----------

// Browser is one known Chromium-family program. Paths are per GOOS and may
// use ~ and %VAR%; Names are looked up on PATH (Linux). Profile is the
// default user data dir, where Chrome 144+ writes DevToolsActivePort when
// remote debugging is enabled from chrome://inspect (no HTTP endpoints then).
type Browser struct {
	Key     string              `json:"key"`
	Name    string              `json:"name"`
	Paths   map[string][]string `json:"paths"`
	Names   []string            `json:"names,omitempty"`
	Profile map[string]string   `json:"profile,omitempty"`
}

var catalog = []Browser{
	{Key: "chrome", Name: "Google Chrome", Paths: map[string][]string{
		"darwin":  {"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", "~/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"},
		"windows": {`%ProgramFiles%\Google\Chrome\Application\chrome.exe`, `%ProgramFiles(x86)%\Google\Chrome\Application\chrome.exe`, `%LOCALAPPDATA%\Google\Chrome\Application\chrome.exe`},
		"linux":   {"/opt/google/chrome/chrome"},
	}, Names: []string{"google-chrome", "google-chrome-stable"}, Profile: map[string]string{
		"darwin": "~/Library/Application Support/Google/Chrome", "windows": `%LOCALAPPDATA%\Google\Chrome\User Data`, "linux": "~/.config/google-chrome",
	}},
	{Key: "edge", Name: "Microsoft Edge", Paths: map[string][]string{
		"darwin":  {"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"},
		"windows": {`%ProgramFiles(x86)%\Microsoft\Edge\Application\msedge.exe`, `%ProgramFiles%\Microsoft\Edge\Application\msedge.exe`},
		"linux":   {"/opt/microsoft/msedge/msedge"},
	}, Names: []string{"microsoft-edge", "microsoft-edge-stable"}, Profile: map[string]string{
		"darwin": "~/Library/Application Support/Microsoft Edge", "windows": `%LOCALAPPDATA%\Microsoft\Edge\User Data`, "linux": "~/.config/microsoft-edge",
	}},
	{Key: "chromium", Name: "Chromium", Paths: map[string][]string{
		"darwin":  {"/Applications/Chromium.app/Contents/MacOS/Chromium", "~/Applications/Chromium.app/Contents/MacOS/Chromium"},
		"windows": {`%LOCALAPPDATA%\Chromium\Application\chrome.exe`},
		"linux":   {"/snap/bin/chromium"},
	}, Names: []string{"chromium", "chromium-browser"}, Profile: map[string]string{
		"darwin": "~/Library/Application Support/Chromium", "windows": `%LOCALAPPDATA%\Chromium\User Data`, "linux": "~/.config/chromium",
	}},
	{Key: "brave", Name: "Brave", Paths: map[string][]string{
		"darwin":  {"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser"},
		"windows": {`%ProgramFiles%\BraveSoftware\Brave-Browser\Application\brave.exe`, `%LOCALAPPDATA%\BraveSoftware\Brave-Browser\Application\brave.exe`},
		"linux":   {"/opt/brave.com/brave/brave"},
	}, Names: []string{"brave-browser"}, Profile: map[string]string{
		"darwin": "~/Library/Application Support/BraveSoftware/Brave-Browser", "windows": `%LOCALAPPDATA%\BraveSoftware\Brave-Browser\User Data`, "linux": "~/.config/BraveSoftware/Brave-Browser",
	}},
	{Key: "chrome-canary", Name: "Chrome Canary", Paths: map[string][]string{
		"darwin":  {"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary"},
		"windows": {`%LOCALAPPDATA%\Google\Chrome SxS\Application\chrome.exe`},
	}, Profile: map[string]string{
		"darwin": "~/Library/Application Support/Google/Chrome Canary", "windows": `%LOCALAPPDATA%\Google\Chrome SxS\User Data`,
	}},
}

// browserProcs are process-name fragments of programs that may serve CDP.
var browserProcs = []string{"chrome", "chromium", "msedge", "microsoft edge", "brave", "比特浏览器", "bitbrowser", "opera", "vivaldi", "electron", "headless_shell"}

// ---------- discovery ----------

type Installed struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// Endpoint is a port answering /json/version.
type Endpoint struct {
	IP              string `json:"ip"`
	Port            int    `json:"port"`
	PID             int32  `json:"pid,omitempty"`
	Process         string `json:"process,omitempty"`
	Browser         string `json:"browser"`
	ProtocolVersion string `json:"protocolVersion,omitempty"`
	UserAgent       string `json:"userAgent,omitempty"`
	// WebSocketDebuggerURL is the browser target through the hub (needs the admin token).
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl,omitempty"`
	LocalWebSocketURL    string `json:"localWebSocketUrl,omitempty"`
	// WSOnly marks a default-profile endpoint enabled from chrome://inspect:
	// no /json/* discovery, and Chrome asks the user to allow each connection.
	WSOnly bool `json:"wsOnly,omitempty"`
}

type browsersResult struct {
	OS        string      `json:"os"`
	Installed []Installed `json:"installed"`
	Running   []Endpoint  `json:"running"`
	// Probed counts listeners checked; Errors are discovery steps that failed.
	Probed int      `json:"probed"`
	Errors []string `json:"errors,omitempty"`
}

func (p *Plugin) agent(w http.ResponseWriter, r *http.Request) (sdk.SessionInfo, bool) {
	info, ok := p.host.Agent(r.PathValue("id"))
	if !ok {
		sdk.WriteError(w, sdk.ErrOffline())
		return info, false
	}
	return info, true
}

func (p *Plugin) handleBrowsers(w http.ResponseWriter, r *http.Request) {
	info, ok := p.agent(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	out := browsersResult{OS: info.OS, Installed: []Installed{}, Running: []Endpoint{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		inst, err := p.findInstalled(r.Context(), id, info.OS)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			out.Errors = append(out.Errors, "fs.which: "+err.Error())
		}
		out.Installed = append(out.Installed, inst...)
	}()
	go func() {
		defer wg.Done()
		eps, n, err := p.findRunning(r.Context(), r, id, r.URL.Query().Get("all") == "1")
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			out.Errors = append(out.Errors, "net.listeners: "+err.Error())
		}
		out.Running, out.Probed = append(out.Running, eps...), n
	}()
	wg.Wait()
	sdk.WriteJSON(w, http.StatusOK, out)
}

func (p *Plugin) findInstalled(ctx context.Context, id, goos string) ([]Installed, error) {
	req := sdk.WhichRequest{}
	owner := map[string]Browser{}
	for _, b := range catalog {
		for _, q := range b.Paths[goos] {
			req.Paths = append(req.Paths, q)
			owner[q] = b
		}
		if goos == "linux" {
			for _, n := range b.Names {
				req.Names = append(req.Names, n)
				owner[n] = b
			}
		}
	}
	raw, err := p.host.Call(ctx, id, sdk.CapFsWhich, req)
	if err != nil {
		return nil, err
	}
	var res sdk.WhichResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	out := []Installed{}
	seen := map[string]bool{}
	for _, h := range res.Hits {
		b := owner[h.Query]
		if seen[b.Key] {
			continue
		}
		seen[b.Key] = true
		out = append(out, Installed{Key: b.Key, Name: b.Name, Path: h.Path})
	}
	return out, nil
}

func (p *Plugin) listeners(ctx context.Context, id string, names []string) ([]sdk.Listener, error) {
	raw, err := p.host.Call(ctx, id, sdk.CapNetListeners, sdk.ListenersRequest{Names: names})
	if err != nil {
		return nil, err
	}
	var ls []sdk.Listener
	err = json.Unmarshal(raw, &ls)
	return ls, err
}

// dialIP maps a listening address to the loopback address the plugin dials.
func dialIP(ip string) (string, bool) {
	switch {
	case ip == "0.0.0.0" || ip == "":
		return "127.0.0.1", true
	case ip == "::":
		return "::1", true
	}
	a := net.ParseIP(ip)
	if a == nil || !a.IsLoopback() {
		return "", false
	}
	if a.To4() != nil {
		return "127.0.0.1", true
	}
	return "::1", true
}

func target(ip string, port int) string { return net.JoinHostPort(ip, strconv.Itoa(port)) }

func (p *Plugin) findRunning(ctx context.Context, r *http.Request, id string, all bool) ([]Endpoint, int, error) {
	names := browserProcs
	if all {
		names = nil
	}
	ls, err := p.listeners(ctx, id, names)
	if err != nil {
		return nil, 0, err
	}
	// One probe per port; IPv4 loopback first.
	type cand struct {
		l  sdk.Listener
		ip string
	}
	byPort := map[int]cand{}
	for _, l := range ls {
		ip, ok := dialIP(l.IP)
		if !ok {
			continue
		}
		if c, had := byPort[l.Port]; !had || (c.ip != "127.0.0.1" && ip == "127.0.0.1") {
			byPort[l.Port] = cand{l, ip}
		}
	}
	cands := make([]cand, 0, len(byPort))
	for _, c := range byPort {
		cands = append(cands, c)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].l.Port < cands[j].l.Port })
	if len(cands) > maxProbe {
		cands = cands[:maxProbe]
	}
	var (
		mu     sync.Mutex
		wg     sync.WaitGroup
		sem    = make(chan struct{}, probeLimit)
		out    []Endpoint
		failed []cand
	)
	for _, c := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ep, err := p.version(ctx, r, id, c.ip, c.l.Port, 2*time.Second)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed = append(failed, c)
				return
			}
			ep.PID, ep.Process = c.l.PID, c.l.Name
			out = append(out, ep)
		}()
	}
	wg.Wait()
	if len(failed) > 0 {
		info, _ := p.host.Agent(id)
		active := p.activePorts(ctx, id, info.OS)
		for _, c := range failed {
			a, ok := active[c.l.Port]
			if !ok {
				continue
			}
			ep := Endpoint{IP: c.ip, Port: c.l.Port, PID: c.l.PID, Process: c.l.Name, Browser: a.browser + "（默认用户目录）", WSOnly: true,
				LocalWebSocketURL: "ws://" + target(c.ip, c.l.Port) + a.path}
			ep.WebSocketDebuggerURL = wsScheme(r) + hubBase(r, id, c.l.Port) + a.path
			out = append(out, ep)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out, len(cands), nil
}

type activePort struct{ browser, path string }

// activePorts reads DevToolsActivePort ("port\n/devtools/browser/<id>") from
// each known browser's default profile.
func (p *Plugin) activePorts(ctx context.Context, id, goos string) map[int]activePort {
	req := sdk.ReadRequest{Limit: 256}
	owner := map[string]Browser{}
	for _, b := range catalog {
		if dir := b.Profile[goos]; dir != "" {
			q := dir + "/DevToolsActivePort"
			if goos == "windows" {
				q = dir + `\DevToolsActivePort`
			}
			req.Paths = append(req.Paths, q)
			owner[q] = b
		}
	}
	out := map[int]activePort{}
	raw, err := p.host.Call(ctx, id, sdk.CapFsRead, req)
	if err != nil {
		return out
	}
	var res sdk.ReadResult
	if json.Unmarshal(raw, &res) != nil {
		return out
	}
	for _, f := range res.Files {
		lines := strings.Fields(string(f.Data))
		if len(lines) < 2 || !strings.HasPrefix(lines[1], "/devtools/browser/") {
			continue
		}
		if port, err := strconv.Atoi(lines[0]); err == nil {
			out[port] = activePort{browser: owner[f.Query].Name, path: lines[1]}
		}
	}
	return out
}

func wsScheme(r *http.Request) string {
	if secure(r) {
		return "wss://"
	}
	return "ws://"
}

type versionDoc struct {
	Browser              string `json:"Browser"`
	ProtocolVersion      string `json:"Protocol-Version"`
	UserAgent            string `json:"User-Agent"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func (p *Plugin) version(ctx context.Context, r *http.Request, id, ip string, port int, timeout time.Duration) (Endpoint, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t := target(ip, port)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+t+"/json/version", nil)
	req.Host = t
	resp, err := p.host.HTTPClient(id, t).Do(req)
	if err != nil {
		return Endpoint{}, err
	}
	defer resp.Body.Close()
	var v versionDoc
	if resp.StatusCode != http.StatusOK {
		return Endpoint{}, fmt.Errorf("/json/version: %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil || v.Browser == "" {
		return Endpoint{}, errors.New("not a DevTools endpoint")
	}
	ep := Endpoint{IP: ip, Port: port, Browser: v.Browser, ProtocolVersion: v.ProtocolVersion, UserAgent: v.UserAgent, LocalWebSocketURL: v.WebSocketDebuggerURL}
	if v.WebSocketDebuggerURL != "" {
		ep.WebSocketDebuggerURL = string(rewrite([]byte(v.WebSocketDebuggerURL), t, hubBase(r, id, port)))
		ep.WebSocketDebuggerURL = wsScheme(r) + strings.TrimPrefix(ep.WebSocketDebuggerURL, "ws://")
	}
	return ep, nil
}

// ---------- launch ----------

type launchRequest struct {
	Browser     string   `json:"browser"` // catalog key; ignored when Path is set
	Path        string   `json:"path"`
	Port        int      `json:"port"` // 0 picks a free one
	Headless    bool     `json:"headless"`
	UserDataDir string   `json:"userDataDir"`
	URL         string   `json:"url"`
	Args        []string `json:"args"`
}

type launchResult struct {
	PID      int      `json:"pid"`
	Path     string   `json:"path"`
	Args     []string `json:"args"`
	Endpoint Endpoint `json:"endpoint"`
}

func (p *Plugin) handleLaunch(w http.ResponseWriter, r *http.Request) {
	info, ok := p.agent(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var req launchRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		sdk.WriteError(w, sdk.ErrBad("invalid json"))
		return
	}
	res, err := p.launch(r, id, info.OS, req)
	detail := fmt.Sprintf("%s port=%d", firstNonEmpty(res.Path, req.Path, req.Browser), res.Endpoint.Port)
	p.host.Audit(r, id, "launch", detail, err)
	if err != nil {
		sdk.WriteError(w, err)
		return
	}
	sdk.WriteJSON(w, http.StatusCreated, res)
}

func (p *Plugin) launch(r *http.Request, id, goos string, req launchRequest) (launchResult, error) {
	ctx := r.Context()
	path, key := strings.TrimSpace(req.Path), req.Browser
	if path == "" {
		inst, err := p.findInstalled(ctx, id, goos)
		if err != nil {
			return launchResult{}, err
		}
		for _, b := range inst {
			if b.Key == key || key == "" {
				path, key = b.Path, b.Key
				break
			}
		}
		if path == "" {
			return launchResult{}, &sdk.Error{Status: http.StatusNotFound, Code: "browser_not_found", Message: "no installed browser matches " + strconv.Quote(req.Browser)}
		}
	}
	if key == "" {
		key = "custom"
	}
	port := req.Port
	if port < 0 || port > 65535 {
		return launchResult{}, sdk.ErrBad("port must be 1-65535")
	}
	if port == 0 {
		ls, err := p.listeners(ctx, id, nil)
		if err != nil {
			return launchResult{}, err
		}
		used := map[int]bool{}
		for _, l := range ls {
			used[l.Port] = true
		}
		for range 50 {
			c := 19222 + rand.IntN(700)
			if !used[c] {
				port = c
				break
			}
		}
		if port == 0 {
			return launchResult{}, errors.New("no free debugging port found")
		}
	}
	dir := strings.TrimSpace(req.UserDataDir)
	if dir == "" {
		dir = "~/.probe-cdp/" + key
	}
	args := []string{
		"--remote-debugging-port=" + strconv.Itoa(port),
		"--user-data-dir=" + dir,
		"--no-first-run",
		"--no-default-browser-check",
	}
	if req.Headless {
		args = append(args, "--headless=new")
	}
	args = append(args, req.Args...)
	if req.URL != "" {
		args = append(args, req.URL)
	}
	raw, err := p.host.Call(ctx, id, sdk.CapProcSpawn, sdk.SpawnRequest{Path: path, Args: args})
	if err != nil {
		return launchResult{Path: path}, err
	}
	var sp sdk.SpawnResult
	_ = json.Unmarshal(raw, &sp)
	res := launchResult{PID: sp.PID, Path: sp.Path, Args: sp.Args}
	deadline := time.Now().Add(launchWait)
	for {
		ep, err := p.version(ctx, r, id, "127.0.0.1", port, 2*time.Second)
		if err == nil {
			ep.PID = int32(sp.PID)
			res.Endpoint = ep
			return res, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			res.Endpoint.Port = port
			return res, &sdk.Error{Status: http.StatusGatewayTimeout, Code: "launch_timeout", Message: fmt.Sprintf(
				"pid %d started but port %d did not answer /json/version within %s; another instance may already use %s (it then ignores the debugging flag)", sp.PID, port, launchWait, dir)}
		}
		time.Sleep(400 * time.Millisecond)
	}
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// ---------- proxy ----------

func portOf(r *http.Request) (int, error) {
	n, err := strconv.Atoi(r.PathValue("port"))
	if err != nil || n < 1 || n > 65535 {
		return 0, sdk.ErrBad("port must be 1-65535")
	}
	return n, nil
}

// hubBase is host+path that replaces "127.0.0.1:{port}" in DevTools URLs, so
// ws://{hubBase}/devtools/... reaches the browser through the hub.
func hubBase(r *http.Request, id string, port int) string {
	return r.Host + sdk.PathPrefix + "/" + Name + "/agents/" + url.PathEscape(id) + "/ports/" + strconv.Itoa(port)
}

func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// rewrite points DevTools URLs at the hub: ws://127.0.0.1:9222/devtools/... and
// the ws=127.0.0.1:9222/devtools/... parameter of devtoolsFrontendUrl.
func rewrite(b []byte, local, base string) []byte {
	b = bytes.ReplaceAll(b, []byte(local+"/devtools/"), []byte(base+"/devtools/"))
	if l, err := url.Parse("http://" + local); err == nil && l.Hostname() == "127.0.0.1" {
		b = bytes.ReplaceAll(b, []byte("localhost:"+l.Port()+"/devtools/"), []byte(base+"/devtools/"))
	}
	return b
}

// ---------- close ----------

func (p *Plugin) handleClose(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	port, err := portOf(r)
	if err != nil {
		sdk.WriteError(w, err)
		return
	}
	err = p.closeBrowser(r.Context(), r, id, port)
	p.host.Audit(r, id, "close", fmt.Sprintf("port=%d", port), err)
	if err != nil {
		sdk.WriteError(w, err)
		return
	}
	sdk.WriteJSON(w, http.StatusOK, map[string]any{"closed": true, "port": port})
}

// closeBrowser sends Browser.close on the browser-level session, so the
// browser shuts down cleanly and flushes its profile. Endpoints without
// /json/version (chrome://inspect on a default profile) are refused: each
// connection there pops a permission dialog on the remote desktop.
func (p *Plugin) closeBrowser(ctx context.Context, r *http.Request, id string, port int) error {
	ep, err := p.version(ctx, r, id, "127.0.0.1", port, 5*time.Second)
	if err != nil {
		return sdk.ErrBad("port has no /json/version; only browsers started with --remote-debugging-port can be closed here")
	}
	u, err := url.Parse(ep.LocalWebSocketURL)
	if err != nil || !strings.HasPrefix(u.Path, "/devtools/browser/") {
		return sdk.ErrBad("browser did not report a browser-level WebSocket")
	}
	t := target("127.0.0.1", port)
	d := websocket.Dialer{
		NetDialContext:   func(ctx context.Context, _, _ string) (net.Conn, error) { return p.host.DialTCP(ctx, id, t) },
		HandshakeTimeout: 10 * time.Second,
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, _, err := d.DialContext(ctx, "ws://"+t+u.Path, nil)
	if err != nil {
		return fmt.Errorf("connect browser: %w", err)
	}
	defer c.Close()
	if err := c.WriteJSON(map[string]any{"id": 1, "method": "Browser.close"}); err != nil {
		return fmt.Errorf("send Browser.close: %w", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var msg struct {
			ID    int `json:"id"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := c.ReadJSON(&msg); err != nil {
			// The browser often exits before replying; a dropped socket is success.
			return nil
		}
		if msg.ID == 1 {
			if msg.Error != nil {
				return fmt.Errorf("Browser.close: %s", msg.Error.Message)
			}
			return nil
		}
	}
}

func (p *Plugin) proxy(id string, port int, path, query string, rewriteJSON func(*http.Response) error) *httputil.ReverseProxy {
	t := target("127.0.0.1", port)
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = t
			pr.Out.URL.Path, pr.Out.URL.RawPath = path, ""
			pr.Out.URL.RawQuery = query
			// DevTools only accepts an IP or localhost Host, and rejects
			// WebSockets with a foreign Origin unless --remote-allow-origins.
			pr.Out.Host = t
			for _, h := range []string{"Origin", "Authorization", "Cookie", "X-Admin-Token", "X-Api-Token", "X-Forwarded-For"} {
				pr.Out.Header.Del(h)
			}
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return p.host.DialTCP(ctx, id, t)
			},
			DisableKeepAlives:     true,
			ResponseHeaderTimeout: 30 * time.Second,
		},
		ModifyResponse: rewriteJSON,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			sdk.WriteError(w, err)
		},
	}
}

func (p *Plugin) handleJSON(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.agent(w, r); !ok {
		return
	}
	port, err := portOf(r)
	if err != nil {
		sdk.WriteError(w, err)
		return
	}
	id := r.PathValue("id")
	path := "/json"
	if rest := r.PathValue("rest"); rest != "" {
		path += "/" + rest
	}
	local, base := target("127.0.0.1", port), hubBase(r, id, port)
	scheme := "ws://"
	if secure(r) {
		scheme = "wss://"
	}
	modify := func(resp *http.Response) error {
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBody))
		resp.Body.Close()
		if err != nil {
			return err
		}
		b = rewrite(b, local, base)
		if scheme == "wss://" {
			b = bytes.ReplaceAll(b, []byte("ws://"+base), []byte("wss://"+base))
		}
		resp.Body = io.NopCloser(bytes.NewReader(b))
		resp.ContentLength = int64(len(b))
		resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
		resp.Header.Set("Cache-Control", "no-store")
		return nil
	}
	p.proxy(id, port, path, r.URL.RawQuery, modify).ServeHTTP(w, r)
}

func (p *Plugin) handleDevtools(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.agent(w, r); !ok {
		return
	}
	port, err := portOf(r)
	if err != nil {
		sdk.WriteError(w, err)
		return
	}
	p.serveWS(w, r, wsTicket{Agent: r.PathValue("id"), Port: port, Path: "/devtools/" + r.PathValue("rest")})
}

func (p *Plugin) serveWS(w http.ResponseWriter, r *http.Request, t wsTicket) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		sdk.WriteError(w, sdk.ErrBad("expected a WebSocket upgrade"))
		return
	}
	p.host.Audit(r, t.Agent, "connect", fmt.Sprintf("port=%d %s", t.Port, t.Path), nil)
	p.proxy(t.Agent, t.Port, t.Path, "", nil).ServeHTTP(w, r)
}

func (p *Plugin) handleTicket(w http.ResponseWriter, r *http.Request) {
	if _, ok := p.agent(w, r); !ok {
		return
	}
	port, err := portOf(r)
	if err != nil {
		sdk.WriteError(w, err)
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		sdk.WriteError(w, sdk.ErrBad("invalid json"))
		return
	}
	if u, err := url.Parse(body.Path); err == nil && u.Scheme != "" {
		body.Path = u.Path
	}
	if !strings.HasPrefix(body.Path, "/devtools/") || strings.Contains(body.Path, "..") {
		sdk.WriteError(w, sdk.ErrBad("path must be a /devtools/... target path"))
		return
	}
	id, exp := p.tickets.Issue(wsTicket{Agent: r.PathValue("id"), Port: port, Path: body.Path})
	sdk.WriteJSON(w, http.StatusOK, map[string]any{
		"ws":      sdk.PublicPrefix + "/" + Name + "/" + id,
		"expires": exp,
		"note":    "one-shot; open within 2 minutes",
	})
}

func (p *Plugin) handleTicketWS(w http.ResponseWriter, r *http.Request) {
	t, ok := p.tickets.Take(r.PathValue("ticket"))
	if !ok {
		http.Error(w, "ticket not found or already used", http.StatusGone)
		return
	}
	p.serveWS(w, r, t)
}

// Catalog lists the browsers the plugin looks for, for docs and tests.
func Catalog() []Browser { return slices.Clone(catalog) }
