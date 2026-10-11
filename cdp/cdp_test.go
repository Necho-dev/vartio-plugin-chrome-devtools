package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Necho-dev/vartio/sdk"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

// fakeHost answers primitives locally and dials targets directly.
type fakeHost struct {
	port   int
	wsOnly int // a listener that only has DevToolsActivePort, like Chrome 144+ default profiles
	mu     sync.Mutex
	spawns []sdk.SpawnRequest
	audits []string
}

func (h *fakeHost) Agents() []sdk.SessionInfo {
	return []sdk.SessionInfo{{DeviceID: "dev1", OS: "darwin"}}
}
func (h *fakeHost) Agent(id string) (sdk.SessionInfo, bool) {
	return sdk.SessionInfo{DeviceID: id, Online: true, OS: "darwin"}, id == "dev1"
}
func (h *fakeHost) DialTCP(ctx context.Context, _, target string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", target)
}
func (h *fakeHost) HTTPClient(id, target string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return h.DialTCP(ctx, id, target)
	}}}
}
func (h *fakeHost) Call(_ context.Context, _, primitive string, params any) (json.RawMessage, error) {
	var out any
	switch primitive {
	case sdk.CapFsWhich:
		req := params.(sdk.WhichRequest)
		out = sdk.WhichResult{Hits: []sdk.WhichHit{{Query: req.Paths[0], Path: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}}}
	case sdk.CapNetListeners:
		ls := []sdk.Listener{{IP: "127.0.0.1", Port: h.port, PID: 42, Name: "Google Chrome"}, {IP: "10.0.0.2", Port: 80, Name: "chrome"}}
		if h.wsOnly > 0 {
			ls = append(ls, sdk.Listener{IP: "127.0.0.1", Port: h.wsOnly, PID: 7, Name: "Google Chrome"})
		}
		out = ls
	case sdk.CapFsRead:
		req := params.(sdk.ReadRequest)
		res := sdk.ReadResult{Files: []sdk.ReadFile{}}
		if h.wsOnly > 0 && strings.HasPrefix(req.Paths[0], "~/Library/Application Support/Google/Chrome/") {
			res.Files = append(res.Files, sdk.ReadFile{Query: req.Paths[0], Data: fmt.Appendf(nil, "%d\n/devtools/browser/abc-123\n", h.wsOnly)})
		}
		out = res
	case sdk.CapProcSpawn:
		req := params.(sdk.SpawnRequest)
		h.mu.Lock()
		h.spawns = append(h.spawns, req)
		h.mu.Unlock()
		out = sdk.SpawnResult{PID: 4242, Path: req.Path, Args: req.Args}
	}
	return json.Marshal(out)
}
func (h *fakeHost) Audit(_ *http.Request, id, kind, detail string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.audits = append(h.audits, id+" "+kind+" "+detail)
}

// fakeChrome mimics DevTools' HTTP endpoints and a page WebSocket that echoes
// what it receives with the Origin and Host it saw.
func fakeChrome(t *testing.T) (port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = ln.Addr().(*net.TCPAddr).Port
	self := fmt.Sprintf("127.0.0.1:%d", port)
	up := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != self {
			http.Error(w, "Host header is specified and is not an IP address or localhost.", http.StatusInternalServerError)
			return
		}
		switch {
		case r.URL.Path == "/json/version":
			fmt.Fprintf(w, `{"Browser":"Chrome/140.0.0.0","Protocol-Version":"1.3","webSocketDebuggerUrl":"ws://%s/devtools/browser/b1"}`, self)
		case r.URL.Path == "/json/list":
			fmt.Fprintf(w, `[{"id":"P1","type":"page","title":"t","url":"about:blank","webSocketDebuggerUrl":"ws://%[1]s/devtools/page/P1","devtoolsFrontendUrl":"/devtools/inspector.html?ws=%[1]s/devtools/page/P1"}]`, self)
		case r.URL.Path == "/devtools/browser/b1":
			c, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			var msg struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if c.ReadJSON(&msg) == nil && msg.Method == "Browser.close" {
				_ = c.WriteJSON(map[string]any{"id": msg.ID, "result": map[string]any{}})
			}
			_ = c.Close()
		case strings.HasPrefix(r.URL.Path, "/devtools/page/"):
			c, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer c.Close()
			for {
				typ, msg, err := c.ReadMessage()
				if err != nil {
					return
				}
				_ = c.WriteMessage(typ, fmt.Appendf(nil, "%s|origin=%q|%s", r.URL.Path, r.Header.Get("Origin"), msg))
			}
		default:
			http.NotFound(w, r)
		}
	})}}
	srv.Start()
	t.Cleanup(srv.Close)
	return port
}

func setup(t *testing.T) (*httptest.Server, *fakeHost, int) {
	t.Helper()
	port := fakeChrome(t)
	host := &fakeHost{port: port}
	p := New()
	if err := p.Init(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	p.Routes(mux, sdk.PathPrefix+"/"+Name)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, host, port
}

func getJSON(t *testing.T, method, url, body string, v any) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if v != nil {
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s %s: %d %s", method, url, resp.StatusCode, b)
		}
	}
	return resp.StatusCode
}

func TestBrowsersDiscovery(t *testing.T) {
	srv, _, port := setup(t)
	base := srv.URL + sdk.PathPrefix + "/cdp/agents/dev1"
	var res browsersResult
	if code := getJSON(t, "GET", base+"/browsers", "", &res); code != 200 {
		t.Fatalf("browsers %d", code)
	}
	if len(res.Installed) != 1 || res.Installed[0].Key != "chrome" {
		t.Fatalf("installed %+v", res.Installed)
	}
	if res.Probed != 1 || len(res.Running) != 1 {
		t.Fatalf("non-loopback listener probed or endpoint missing: %+v", res)
	}
	ep := res.Running[0]
	want := "ws://" + strings.TrimPrefix(srv.URL, "http://") + sdk.PathPrefix + "/cdp/agents/dev1/ports/" + strconv.Itoa(port) + "/devtools/browser/b1"
	if ep.Port != port || ep.PID != 42 || ep.WebSocketDebuggerURL != want || !strings.HasPrefix(ep.LocalWebSocketURL, "ws://127.0.0.1:") {
		t.Fatalf("endpoint %+v\nwant ws %s", ep, want)
	}
}

func TestWSOnlyEndpoint(t *testing.T) {
	srv, host, _ := setup(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(dead.Close)
	host.wsOnly = dead.Listener.Addr().(*net.TCPAddr).Port
	var res browsersResult
	getJSON(t, "GET", srv.URL+sdk.PathPrefix+"/cdp/agents/dev1/browsers", "", &res)
	if len(res.Running) != 2 {
		t.Fatalf("running %+v", res.Running)
	}
	var ep Endpoint
	for _, e := range res.Running {
		if e.Port == host.wsOnly {
			ep = e
		}
	}
	if !ep.WSOnly || !strings.HasSuffix(ep.WebSocketDebuggerURL, fmt.Sprintf("/ports/%d/devtools/browser/abc-123", host.wsOnly)) || ep.Browser != "Google Chrome（默认用户目录）" {
		t.Fatalf("ws-only endpoint %+v", ep)
	}
}

func TestCloseBrowser(t *testing.T) {
	srv, host, port := setup(t)
	var res map[string]any
	if code := getJSON(t, "POST", fmt.Sprintf("%s%s/cdp/agents/dev1/ports/%d/close", srv.URL, sdk.PathPrefix, port), "", &res); code != http.StatusOK || res["closed"] != true {
		t.Fatalf("close: %d %v", code, res)
	}
	host.mu.Lock()
	audits := slices.Clone(host.audits)
	host.mu.Unlock()
	if len(audits) != 1 || !strings.Contains(audits[0], "close port="+strconv.Itoa(port)) {
		t.Fatalf("audits %v", audits)
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(dead.Close)
	if code := getJSON(t, "POST", fmt.Sprintf("%s%s/cdp/agents/dev1/ports/%d/close", srv.URL, sdk.PathPrefix, dead.Listener.Addr().(*net.TCPAddr).Port), "", nil); code != http.StatusBadRequest {
		t.Fatalf("close ws-only/dead port: %d", code)
	}
}

func TestJSONRewrite(t *testing.T) {
	srv, _, port := setup(t)
	var list []map[string]string
	getJSON(t, "GET", fmt.Sprintf("%s%s/cdp/agents/dev1/ports/%d/json/list", srv.URL, sdk.PathPrefix, port), "", &list)
	hub := strings.TrimPrefix(srv.URL, "http://") + fmt.Sprintf("%s/cdp/agents/dev1/ports/%d/devtools/page/P1", sdk.PathPrefix, port)
	if len(list) != 1 || list[0]["webSocketDebuggerUrl"] != "ws://"+hub || !strings.HasSuffix(list[0]["devtoolsFrontendUrl"], "ws="+hub) {
		t.Fatalf("rewrite %+v", list)
	}
	if code := getJSON(t, "GET", srv.URL+sdk.PathPrefix+"/cdp/agents/dev1/ports/0/json/list", "", nil); code != http.StatusBadRequest {
		t.Fatalf("port 0: %d", code)
	}
	if code := getJSON(t, "GET", srv.URL+sdk.PathPrefix+"/cdp/agents/nope/ports/9222/json/list", "", nil); code != http.StatusConflict {
		t.Fatalf("offline agent: %d", code)
	}
}

func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	hdr := http.Header{"Origin": {"http://hub.example"}}
	c, resp, err := websocket.DefaultDialer.Dial(url, hdr)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (%d)", url, err, code)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func roundTrip(t *testing.T, c *websocket.Conn, msg string) string {
	t.Helper()
	if err := c.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
		t.Fatal(err)
	}
	_, got, err := c.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

func TestWebSocketProxy(t *testing.T) {
	srv, host, port := setup(t)
	ws := "ws" + strings.TrimPrefix(srv.URL, "http") + fmt.Sprintf("%s/cdp/agents/dev1/ports/%d/devtools/page/P1", sdk.PathPrefix, port)
	c := dialWS(t, ws)
	msg := `{"id":1,"method":"Runtime.evaluate"}`
	if got := roundTrip(t, c, msg); got != `/devtools/page/P1|origin=""|`+msg {
		t.Fatalf("echo %q", got)
	}
	big := strings.Repeat("x", 3<<20)
	if got := roundTrip(t, c, big); !strings.HasSuffix(got, big) {
		t.Fatalf("large frame: %d bytes back", len(got))
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.audits) != 1 || !strings.Contains(host.audits[0], "connect port="+strconv.Itoa(port)+" /devtools/page/P1") {
		t.Fatalf("audit %v", host.audits)
	}
}

func TestTicket(t *testing.T) {
	srv, _, port := setup(t)
	url := fmt.Sprintf("%s%s/cdp/agents/dev1/ports/%d/ticket", srv.URL, sdk.PathPrefix, port)
	if code := getJSON(t, "POST", url, `{"path":"/json/list"}`, nil); code != http.StatusBadRequest {
		t.Fatalf("non-devtools path: %d", code)
	}
	var tk struct{ WS string }
	full := "ws://hub/devtools/page/P1"
	if code := getJSON(t, "POST", url, `{"path":"`+full+`"}`, &tk); code != 200 || !strings.HasPrefix(tk.WS, sdk.PublicPrefix+"/cdp/") {
		t.Fatalf("ticket %d %+v", code, tk)
	}
	ws := "ws" + strings.TrimPrefix(srv.URL, "http") + tk.WS
	c := dialWS(t, ws)
	if got := roundTrip(t, c, "hi"); got != `/devtools/page/P1|origin=""|hi` {
		t.Fatalf("echo %q", got)
	}
	if _, resp, err := websocket.DefaultDialer.Dial(ws, nil); err == nil || resp == nil || resp.StatusCode != http.StatusGone {
		t.Fatalf("ticket reused: %v", err)
	}
}

func TestLaunch(t *testing.T) {
	srv, host, port := setup(t)
	var res launchResult
	body := fmt.Sprintf(`{"browser":"chrome","port":%d,"headless":true,"url":"https://example.com"}`, port)
	if code := getJSON(t, "POST", srv.URL+sdk.PathPrefix+"/cdp/agents/dev1/launch", body, &res); code != http.StatusCreated {
		t.Fatalf("launch %d %+v", code, res)
	}
	if res.PID != 4242 || res.Endpoint.Port != port || res.Endpoint.Browser != "Chrome/140.0.0.0" {
		t.Fatalf("result %+v", res)
	}
	host.mu.Lock()
	sp := host.spawns[0]
	host.mu.Unlock()
	args := strings.Join(sp.Args, " ")
	for _, want := range []string{"--remote-debugging-port=" + strconv.Itoa(port), "--user-data-dir=~/.probe-cdp/chrome", "--headless=new"} {
		if !strings.Contains(args, want) {
			t.Fatalf("args %q missing %q", args, want)
		}
	}
	if !strings.HasSuffix(sp.Path, "Google Chrome") || sp.Args[len(sp.Args)-1] != "https://example.com" {
		t.Fatalf("spawn %+v", sp)
	}
	if code := getJSON(t, "POST", srv.URL+sdk.PathPrefix+"/cdp/agents/dev1/launch", `{"browser":"netscape"}`, nil); code != http.StatusNotFound {
		t.Fatalf("unknown browser: %d", code)
	}
}

func TestDialIP(t *testing.T) {
	for in, want := range map[string]string{"0.0.0.0": "127.0.0.1", "127.0.0.1": "127.0.0.1", "::": "::1", "::1": "::1", "10.1.1.1": ""} {
		got, _ := dialIP(in)
		if got != want {
			t.Errorf("%s: %q want %q", in, got, want)
		}
	}
}
