package webui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crossparties/tether/internal/core"
	"github.com/crossparties/tether/internal/harness"
	"github.com/crossparties/tether/internal/mount"
	"github.com/crossparties/tether/internal/snapshot"
)

type testWebClient struct {
	t      *testing.T
	base   string
	client *http.Client
	cookie *http.Cookie
	csrf   string
}

func newTestWebServer(t *testing.T, ws string) (*Server, *httptest.Server, *testWebClient) {
	t.Helper()
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	srv, err := NewServer(Config{Engine: eng})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		ts.Close()
	})

	wc := &testWebClient{
		t:    t,
		base: ts.URL,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	return srv, ts, wc
}

func (c *testWebClient) redeemBootstrap(token string) {
	c.t.Helper()
	resp, err := c.client.Get(c.base + "/?token=" + token)
	if err != nil {
		c.t.Fatalf("GET /?token=: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		c.t.Fatalf("expected 302 redirect on bootstrap redeem, got %d: %s", resp.StatusCode, string(body))
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		c.t.Fatalf("expected redirect to /, got %q", loc)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		c.t.Fatalf("expected Cache-Control: no-store, got %q", cc)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookieName {
			c.cookie = ck
		}
	}
	if c.cookie == nil {
		c.t.Fatal("missing tether_session cookie")
	}
	if !c.cookie.HttpOnly || c.cookie.SameSite != http.SameSiteStrictMode {
		c.t.Fatalf("expected HttpOnly + SameSite=Strict cookie, got HttpOnly=%v SameSite=%v", c.cookie.HttpOnly, c.cookie.SameSite)
	}

	sResp := c.do(http.MethodGet, "/api/v1/auth/session", nil, nil)
	defer sResp.Body.Close()
	if sResp.StatusCode != http.StatusOK {
		c.t.Fatalf("GET /api/v1/auth/session failed: %d", sResp.StatusCode)
	}
	var payload struct {
		Authenticated bool   `json:"authenticated"`
		CSRFToken     string `json:"csrf_token"`
	}
	if err := json.NewDecoder(sResp.Body).Decode(&payload); err != nil {
		c.t.Fatalf("decode auth/session: %v", err)
	}
	if !payload.Authenticated || payload.CSRFToken == "" {
		c.t.Fatalf("unexpected auth/session payload: %+v", payload)
	}
	c.csrf = payload.CSRFToken
}

func (c *testWebClient) do(method, path string, body any, headers map[string]string) *http.Response {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, r)
	if err != nil {
		c.t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != nil {
		req.AddCookie(c.cookie)
	}
	if c.csrf != "" && isMutatingMethod(method) {
		req.Header.Set(csrfHeaderName, c.csrf)
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s failed: %v", method, path, err)
	}
	return resp
}

func TestAuthCSRFOriginAndBoundarySecurity(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("API_SECRET=topsecret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("hello workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "public"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "public", "index.html"), []byte("<h1>Hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outsideFile, []byte("outside secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Symlink(outsideFile, filepath.Join(ws, "public", "leak.html"))

	srv, ts, wc := newTestWebServer(t, ws)
	tok := srv.BootstrapToken()

	// 1. Unauthenticated requests MUST be rejected with 401.
	for _, ep := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/health"},
		{http.MethodGet, "/api/v1/auth/session"},
		{http.MethodGet, "/api/v1/project"},
		{http.MethodGet, "/api/v1/events"},
		{http.MethodGet, "/preview/public/index.html"},
		{http.MethodPost, "/api/v1/chat/send"},
	} {
		resp := wc.do(ep.method, ep.path, nil, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s %s unauthenticated: expected 401, got %d", ep.method, ep.path, resp.StatusCode)
		}
	}

	// 2. Single-use ?token= redemption works once and rejects 2nd reuse.
	wc.redeemBootstrap(tok)

	reuseReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/?token="+tok, nil)
	reuseResp, err := wc.client.Do(reuseReq)
	if err != nil {
		t.Fatal(err)
	}
	reuseResp.Body.Close()
	if reuseResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on 2nd ?token= reuse, got %d", reuseResp.StatusCode)
	}

	// Also verify /auth without valid unused token fails 401.
	authReuseResp, err := wc.client.Get(ts.URL + "/auth?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	authReuseResp.Body.Close()
	if authReuseResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on /auth token reuse, got %d", authReuseResp.StatusCode)
	}

	// 3. Wrong Origin rejected 403.
	for _, badOrigin := range []string{"https://evil.com", "http://127.0.0.1:9999", "null"} {
		resp := wc.do(http.MethodGet, "/api/v1/project", nil, map[string]string{"Origin": badOrigin})
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("Origin=%q: expected 403, got %d", badOrigin, resp.StatusCode)
		}
	}

	// DNS rebinding Host rejected 403.
	rebReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/project", nil)
	rebReq.Host = "evil.com"
	rebReq.AddCookie(wc.cookie)
	rebResp, err := wc.client.Do(rebReq)
	if err != nil {
		t.Fatal(err)
	}
	rebResp.Body.Close()
	if rebResp.StatusCode != http.StatusForbidden {
		t.Fatalf("Host=evil.com: expected 403, got %d", rebResp.StatusCode)
	}

	// 4. Mutating REST without X-Tether-Token rejected 403.
	noCSRF := wc.do(http.MethodPost, "/api/v1/threads", map[string]string{"title": "t"}, map[string]string{
		csrfHeaderName: "",
	})
	noCSRF.Body.Close()
	if noCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without X-Tether-Token: expected 403, got %d", noCSRF.StatusCode)
	}

	badCSRF := wc.do(http.MethodPost, "/api/v1/threads", map[string]string{"title": "t"}, map[string]string{
		csrfHeaderName: "wrong-csrf",
	})
	badCSRF.Body.Close()
	if badCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("POST with wrong X-Tether-Token: expected 403, got %d", badCSRF.StatusCode)
	}

	// 5. Protected file and sandboxed preview boundary enforcement.
	for _, blockedPath := range []string{".env", ".tether/session.db", "../outside.txt", "public/leak.html"} {
		r1 := wc.do(http.MethodGet, "/api/v1/files/content?path="+blockedPath, nil, nil)
		r1.Body.Close()
		if r1.StatusCode != http.StatusForbidden {
			t.Fatalf("/api/v1/files/content?path=%s: expected 403, got %d", blockedPath, r1.StatusCode)
		}
		r2 := wc.do(http.MethodGet, "/preview/"+blockedPath, nil, nil)
		r2.Body.Close()
		if r2.StatusCode != http.StatusForbidden {
			t.Fatalf("/preview/%s: expected 403, got %d", blockedPath, r2.StatusCode)
		}
	}

	// Valid non-ignored preview serves with nosniff + sandbox CSP.
	pResp := wc.do(http.MethodGet, "/preview/public/index.html", nil, nil)
	defer pResp.Body.Close()
	if pResp.StatusCode != http.StatusOK {
		t.Fatalf("/preview/public/index.html expected 200, got %d", pResp.StatusCode)
	}
	if pResp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("missing nosniff header: %q", pResp.Header.Get("X-Content-Type-Options"))
	}
	if csp := pResp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Fatalf("expected sandbox CSP header, got %q", csp)
	}
}

func TestMultiplexedSSELastEventIDReplayAndLiveStream(t *testing.T) {
	ws := t.TempDir()
	srv, ts, wc := newTestWebServer(t, ws)
	wc.redeemBootstrap(srv.BootstrapToken())

	srv.hub.Publish("chat.delta", map[string]any{"text": "first"})
	srv.hub.Publish("keys.switched", "key-1 -> key-2")
	srv.hub.Publish("plan.updated", map[string]any{"mode": "plan"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/v1/events", nil)
	req.AddCookie(wc.cookie)
	req.Header.Set("Last-Event-ID", "1")
	req.Header.Set("Origin", ts.URL)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 SSE, got %d", resp.StatusCode)
	}

	type sseMsg struct {
		id    string
		event string
		data  string
	}
	msgCh := make(chan sseMsg, 10)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		var cur sseMsg
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if cur.event != "" || cur.data != "" {
					msgCh <- cur
					cur = sseMsg{}
				}
				continue
			}
			if v, ok := strings.CutPrefix(line, "id:"); ok {
				cur.id = strings.TrimSpace(v)
			} else if v, ok := strings.CutPrefix(line, "event:"); ok {
				cur.event = strings.TrimSpace(v)
			} else if v, ok := strings.CutPrefix(line, "data:"); ok {
				cur.data = strings.TrimSpace(v)
			}
		}
		_ = sc.Err()
	}()

	var got []sseMsg
	for range 3 {
		select {
		case m := <-msgCh:
			got = append(got, m)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for SSE replay frames; got %+v", got)
		}
	}
	if got[0].event != "connected" {
		t.Fatalf("expected initial connected event, got %+v", got[0])
	}
	if got[1].id != "2" || got[1].event != "keys.switched" {
		t.Fatalf("expected replay event id=2 keys.switched, got %+v", got[1])
	}
	if got[2].id != "3" || got[2].event != "plan.updated" {
		t.Fatalf("expected replay event id=3 plan.updated, got %+v", got[2])
	}

	// Live publish ID 4.
	srv.hub.Publish("mount.updated", map[string]any{"path": "main.go"})
	select {
	case m := <-msgCh:
		if m.id != "4" || m.event != "mount.updated" {
			t.Fatalf("expected live event id=4 mount.updated, got %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for live SSE event")
	}
}

func TestRESTIgnoreSkillsMemoryPlanKeysDoctorAndProjectOpen(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "notes.md"), []byte("# Secret Note\nsecret_token_alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".gitignore"), []byte("dist/\n*.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, _, wc := newTestWebServer(t, ws)
	wc.redeemBootstrap(srv.BootstrapToken())

	// Index notes.md in cache.db first so we verify POST /api/v1/ignore triggers ReconcileKB!
	eng, _ := srv.activeState()
	if _, err := eng.IndexDirectory(ws, nil); err != nil {
		t.Fatal(err)
	}

	// Test ignore impact & save ignore rule covering notes.md -> purges cache.db!
	testResp := wc.do(http.MethodPost, "/api/v1/ignore/test", map[string]any{"pattern": "*.go"}, nil)
	var sim mount.SimulationResult
	_ = json.NewDecoder(testResp.Body).Decode(&sim)
	testResp.Body.Close()
	if sim.MatchedCount == 0 || len(sim.Warnings) == 0 {
		t.Fatalf("expected ignore simulation to warn on *.go, got %+v", sim)
	}

	convResp := wc.do(http.MethodPost, "/api/v1/ignore/convert-gitignore", map[string]any{"apply": false}, nil)
	var convOut struct {
		Converted string   `json:"converted"`
		Skipped   []string `json:"skipped"`
	}
	_ = json.NewDecoder(convResp.Body).Decode(&convOut)
	convResp.Body.Close()
	if !strings.Contains(convOut.Converted, "dist/") || len(convOut.Skipped) == 0 {
		t.Fatalf("expected dist/ converted and *.go skipped, got %+v", convOut)
	}

	igResp := wc.do(http.MethodPost, "/api/v1/ignore", map[string]any{"content": "notes.md\n"}, nil)
	var igOut struct {
		OK       bool `json:"ok"`
		PurgedKB int  `json:"purged_kb"`
	}
	_ = json.NewDecoder(igResp.Body).Decode(&igOut)
	igResp.Body.Close()
	if igResp.StatusCode != http.StatusOK || !igOut.OK || igOut.PurgedKB == 0 {
		t.Fatalf("expected POST /api/v1/ignore to purge notes.md from KB, got status=%d %+v", igResp.StatusCode, igOut)
	}

	// Mount mode override refuses .env, allows regular files.
	mBad := wc.do(http.MethodPatch, "/api/v1/mounts/mode", map[string]any{"path": ".env", "access_mode": "rw"}, nil)
	mBad.Body.Close()
	if mBad.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 downgrading .env, got %d", mBad.StatusCode)
	}
	mOK := wc.do(http.MethodPatch, "/api/v1/mounts/mode", map[string]any{"path": "main.go", "access_mode": "guarded"}, nil)
	mOK.Body.Close()
	if mOK.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 mounting main.go guarded, got %d", mOK.StatusCode)
	}

	// Skills confinement + symlink rejection.
	skPut := wc.do(http.MethodPut, "/api/v1/skills", map[string]any{"name": "go-review", "content": "---\ndescription: Review Go code\n---\nCheck errors.\n"}, nil)
	skPut.Body.Close()
	if skPut.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/v1/skills failed: %d", skPut.StatusCode)
	}
	skBad := wc.do(http.MethodPut, "/api/v1/skills", map[string]any{"name": "../evil", "content": "x"}, nil)
	skBad.Body.Close()
	if skBad.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for ../evil skill name, got %d", skBad.StatusCode)
	}
	_ = os.Symlink("/etc/passwd", filepath.Join(ws, ".agents", "skills", "sym-skill"))
	skSym := wc.do(http.MethodPut, "/api/v1/skills", map[string]any{"name": "sym-skill", "content": "x"}, nil)
	skSym.Body.Close()
	if skSym.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for symlinked skill dir, got %d", skSym.StatusCode)
	}
	skList := wc.do(http.MethodGet, "/api/v1/skills", nil, nil)
	var skOut struct {
		Skills []struct {
			Name string `json:"name"`
		} `json:"skills"`
	}
	_ = json.NewDecoder(skList.Body).Decode(&skOut)
	skList.Body.Close()
	if len(skOut.Skills) != 1 || skOut.Skills[0].Name != "go-review" {
		t.Fatalf("expected only go-review skill, got %+v", skOut)
	}

	// Plan + StartBuilding.
	pPut := wc.do(http.MethodPut, "/api/v1/plan", map[string]any{"content": "# Plan\n1. Build feature\n"}, nil)
	pPut.Body.Close()
	if pPut.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/v1/plan failed: %d", pPut.StatusCode)
	}
	sbResp := wc.do(http.MethodPost, "/api/v1/plan/start-building", map[string]any{"run_critic": false}, nil)
	sbResp.Body.Close()
	if sbResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/plan/start-building failed: %d", sbResp.StatusCode)
	}

	// Memory CRUD.
	memAdd := wc.do(http.MethodPost, "/api/v1/memory", map[string]any{"kind": "decision", "topic": "auth", "decision": "use SameSite=Strict"}, nil)
	memAdd.Body.Close()
	if memAdd.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/memory failed: %d", memAdd.StatusCode)
	}
	memGet := wc.do(http.MethodGet, "/api/v1/memory", nil, nil)
	var memBody struct {
		Decisions []core.Decision `json:"decisions"`
	}
	_ = json.NewDecoder(memGet.Body).Decode(&memBody)
	memGet.Body.Close()
	if len(memBody.Decisions) == 0 {
		t.Fatal("expected decision in GET /api/v1/memory")
	}
	memDel := wc.do(http.MethodDelete, "/api/v1/memory", map[string]any{"kind": "decision", "id": memBody.Decisions[0].ID}, nil)
	memDel.Body.Close()
	if memDel.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/v1/memory failed: %d", memDel.StatusCode)
	}

	// Keys PUT masks secrets cleanly and preserves masked updates.
	kPut := wc.do(http.MethodPut, "/api/v1/keys", map[string]any{"provider": "deepseek", "api_key": "sk-secret-abc"}, nil)
	rawKBytes, _ := io.ReadAll(kPut.Body)
	kPut.Body.Close()
	if kPut.StatusCode != http.StatusOK || strings.Contains(string(rawKBytes), "sk-secret-abc") || !strings.Contains(string(rawKBytes), "**********") {
		t.Fatalf("PUT /api/v1/keys leaked or failed: status=%d body=%s", kPut.StatusCode, string(rawKBytes))
	}

	// Doctor read-only vs fix.
	docRO := wc.do(http.MethodPost, "/api/v1/doctor", map[string]any{"fix": false}, nil)
	docRO.Body.Close()
	if docRO.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/doctor failed: %d", docRO.StatusCode)
	}

	// Project open switches cleanly.
	ws2 := t.TempDir()
	openResp := wc.do(http.MethodPost, "/api/v1/project/open", map[string]any{"path": ws2, "init": true}, nil)
	var openProj map[string]any
	_ = json.NewDecoder(openResp.Body).Decode(&openProj)
	openResp.Body.Close()
	if openResp.StatusCode != http.StatusOK || openProj["workspace"] != ws2 {
		t.Fatalf("POST /api/v1/project/open failed: status=%d body=%+v", openResp.StatusCode, openProj)
	}
}

func TestSPAPlaceholderSetupBrowseSettingsAndUndoTurn(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(ws, "app.txt")
	if err := os.WriteFile(target, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv, _, wc := newTestWebServer(t, ws)
	wc.redeemBootstrap(srv.BootstrapToken())

	// Embedded SPA shell + SPA fallback route.
	for _, route := range []string{"/", "/settings"} {
		resp := wc.do(http.MethodGet, route, nil, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Tether Local Web Server") {
			t.Fatalf("GET %s failed: status=%d body=%s", route, resp.StatusCode, string(body))
		}
	}

	// Setup status, probe, browse, threads, settings, mcp/import.
	stResp := wc.do(http.MethodGet, "/api/v1/setup/status", nil, nil)
	stRaw, _ := io.ReadAll(stResp.Body)
	stResp.Body.Close()
	if stResp.StatusCode != http.StatusOK || !strings.Contains(string(stRaw), "serena_suggestion") {
		t.Fatalf("GET /api/v1/setup/status failed (%d): %s", stResp.StatusCode, string(stRaw))
	}

	probeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer probeSrv.Close()
	pResp := wc.do(http.MethodPost, "/api/v1/keys/probe", map[string]any{
		"provider": "deepseek",
		"api_key":  "sk-probe",
		"base_url": probeSrv.URL + "/chat/completions",
	}, nil)
	pResp.Body.Close()
	if pResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/keys/probe failed: %d", pResp.StatusCode)
	}

	bResp := wc.do(http.MethodGet, "/api/v1/project/browse", nil, nil)
	bResp.Body.Close()
	if bResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/project/browse failed: %d", bResp.StatusCode)
	}

	thCreate := wc.do(http.MethodPost, "/api/v1/threads", map[string]any{"title": "Review Thread"}, nil)
	var th core.ConversationThread
	_ = json.NewDecoder(thCreate.Body).Decode(&th)
	thCreate.Body.Close()
	if thCreate.StatusCode != http.StatusCreated || th.ID == "" {
		t.Fatalf("POST /api/v1/threads failed: status=%d %+v", thCreate.StatusCode, th)
	}
	thEvents := wc.do(http.MethodGet, "/api/v1/threads/"+th.ID+"/events", nil, nil)
	thEvents.Body.Close()
	if thEvents.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/threads/%s/events failed: %d", th.ID, thEvents.StatusCode)
	}

	setPut := wc.do(http.MethodPut, "/api/v1/settings", map[string]any{
		"mode": "plan",
		"project_settings": map[string]string{
			"api_key":  "must-never-store",
			"test_cmd": "go test ./...",
		},
	}, nil)
	setRaw, _ := io.ReadAll(setPut.Body)
	setPut.Body.Close()
	if setPut.StatusCode != http.StatusOK || strings.Contains(string(setRaw), "must-never-store") {
		t.Fatalf("PUT /api/v1/settings failed or leaked api_key (%d): %s", setPut.StatusCode, string(setRaw))
	}

	impResp := wc.do(http.MethodPost, "/api/v1/mcp/import", map[string]any{"source": "all", "scope": "global"}, nil)
	impResp.Body.Close()
	if impResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/mcp/import failed: %d", impResp.StatusCode)
	}

	// Undo turn conflict 409 -> force: true restore.
	store := snapshot.New(ws)
	if err := store.Capture("t-1", target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("agent-edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPostHash("t-1", target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("manual-edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	utConflict := wc.do(http.MethodPost, "/api/v1/diffs/undo-turn", map[string]any{"turn": "t-1", "force": false}, nil)
	utConflict.Body.Close()
	if utConflict.StatusCode != http.StatusConflict {
		t.Fatalf("POST /api/v1/diffs/undo-turn expected 409, got %d", utConflict.StatusCode)
	}
	utForce := wc.do(http.MethodPost, "/api/v1/diffs/undo-turn", map[string]any{"turn": "t-1", "force": true}, nil)
	utForce.Body.Close()
	if utForce.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/diffs/undo-turn force=true expected 200, got %d", utForce.StatusCode)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "before\n" {
		t.Fatalf("expected restored 'before\\n', got %q", string(got))
	}

	// Abort when idle succeeds cleanly.
	abResp := wc.do(http.MethodPost, "/api/v1/chat/abort", map[string]any{}, nil)
	abResp.Body.Close()
	if abResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/chat/abort failed: %d", abResp.StatusCode)
	}
}

func TestPreviewSkillsMemoryDiffIgnoreAndAbortEdgeCases(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, ".env"), []byte("API_SECRET=topsecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "secrets", "key.txt"), []byte("hidden-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".tetherignore"), []byte("secrets/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "app.js"), []byte("console.log('ok');\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "backup_target.txt"), []byte("SECRET_OLD_BACKUP\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".gitignore"), []byte("dist/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(ws, "blocked.txt"), []byte("orig\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mockAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			chunks := []string{
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc_block","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"blocked.txt\",\"content\":\"wait-for-abort\"}"}}]}}]}`,
				`data: {"choices":[{"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}
			for _, line := range chunks {
				_, _ = fmt.Fprint(w, line+"\n\n")
				if fl != nil {
					fl.Flush()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id":   "tc_block",
						"type": "function",
						"function": map[string]any{
							"name":      "write_file",
							"arguments": `{"path":"blocked.txt","content":"wait-for-abort"}`,
						},
					}},
				},
				"finish_reason": "tool_calls",
			}},
		})
	}))
	defer mockAI.Close()

	eng, err := core.NewEngine(ws)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	run := harness.NewRunner(eng, harness.Config{
		Provider: "deepseek",
		Model:    "deepseek-chat",
		BaseURL:  mockAI.URL,
		APIKey:   "sk-abort-test",
		Out:      &bytes.Buffer{},
	})
	defer func() { _ = run.Close() }()

	srv, err := NewServer(Config{Engine: eng, Runner: run, RunnerConfig: harness.Config{
		Provider: "deepseek",
		Model:    "deepseek-chat",
		BaseURL:  mockAI.URL,
		APIKey:   "sk-abort-test",
	}})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		ts.Close()
	}()

	wc := &testWebClient{
		t:    t,
		base: ts.URL,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	wc.redeemBootstrap(srv.BootstrapToken())

	// 1. Negative preview security checks: ..%2F traversal, AccessIgnored blocking, and valid /api/v1/preview/{path...}.
	for _, bad := range []string{
		"/preview/sub/..%2F.env",
		"/preview/%2e%2e/.env",
		"/api/v1/preview/sub/..%2F.env",
		"/preview/secrets/key.txt",
		"/api/v1/preview/secrets/key.txt",
	} {
		resp := wc.do(http.MethodGet, bad, nil, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for %s, got %d", bad, resp.StatusCode)
		}
	}
	okPrev := wc.do(http.MethodGet, "/api/v1/preview/app.js", nil, nil)
	okPrevBody, _ := io.ReadAll(okPrev.Body)
	okPrev.Body.Close()
	if okPrev.StatusCode != http.StatusOK || !strings.Contains(string(okPrevBody), "console.log('ok')") {
		t.Fatalf("expected 200 valid file on /api/v1/preview/app.js, got %d %s", okPrev.StatusCode, string(okPrevBody))
	}

	// 2. PUT /api/v1/skills rejects "." / ".." with explicit traversal error (never "authorized").
	for _, badName := range []string{".", "..", "bad..name"} {
		sk := wc.do(http.MethodPut, "/api/v1/skills", map[string]any{"name": badName, "content": "x"}, nil)
		body, _ := io.ReadAll(sk.Body)
		sk.Body.Close()
		if sk.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "invalid path traversal") {
			t.Fatalf("expected 400 traversal error for %q, got %d: %s", badName, sk.StatusCode, string(body))
		}
	}

	// 3. EventHub.Publish compacts multi-line JSON payloads into valid single-line SSE data frames.
	sseResp := wc.do(http.MethodGet, "/api/v1/events", nil, nil)
	defer sseResp.Body.Close()
	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/events failed: %d", sseResp.StatusCode)
	}
	type sseFrame struct{ id, event, data string }
	evCh := make(chan sseFrame, 32)
	go func() {
		sc := bufio.NewScanner(sseResp.Body)
		var cur sseFrame
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if cur.event != "" || cur.data != "" {
					evCh <- cur
				}
				cur = sseFrame{}
				continue
			}
			switch {
			case strings.HasPrefix(line, "id: "):
				cur.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				cur.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				cur.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()

	srv.hub.Publish("plan.updated", json.RawMessage("{\n  \"mode\": \"build\",\n  \"nested\": {\n    \"ok\": true\n  }\n}"))
	deadline := time.After(3 * time.Second)
	sawCompacted := false
	for !sawCompacted {
		select {
		case msg := <-evCh:
			if msg.event == "plan.updated" && strings.Contains(msg.data, `"mode":"build"`) && !strings.Contains(msg.data, "\n") {
				sawCompacted = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for compacted multi-line JSON SSE event")
		}
	}

	// 4. Snapshot backup of a file later added to .tether/ignore must be redacted in GET /api/v1/diffs.
	target := filepath.Join(ws, "backup_target.txt")
	store := snapshot.New(ws)
	if err := store.Capture("turn-redact", target); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("SECRET_NEW_CONTENT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPostHash("turn-redact", target); err != nil {
		t.Fatal(err)
	}
	igPost := wc.do(http.MethodPost, "/api/v1/ignore", map[string]any{"content": "secrets/\nbackup_target.txt\n"}, nil)
	igPost.Body.Close()
	if igPost.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/ignore failed: %d", igPost.StatusCode)
	}
	diffsResp := wc.do(http.MethodGet, "/api/v1/diffs", nil, nil)
	diffsRaw, _ := io.ReadAll(diffsResp.Body)
	diffsResp.Body.Close()
	if diffsResp.StatusCode != http.StatusOK || strings.Contains(string(diffsRaw), "SECRET_OLD_BACKUP") || !strings.Contains(string(diffsRaw), `"redacted":true`) {
		t.Fatalf("GET /api/v1/diffs leaked or failed to redact ignored backup (%d): %s", diffsResp.StatusCode, string(diffsRaw))
	}

	// 5. Optional body helpers: invalid JSON in convert-gitignore surfaces 400; empty chunked body works; DELETE memory preserves ?kind=pattern.
	badJSONReq, _ := http.NewRequest(http.MethodPost, wc.base+"/api/v1/ignore/convert-gitignore", strings.NewReader("{bad"))
	badJSONReq.Header.Set("Content-Type", "application/json")
	badJSONReq.AddCookie(wc.cookie)
	badJSONReq.Header.Set(csrfHeaderName, wc.csrf)
	badJSONResp, err := wc.client.Do(badJSONReq)
	if err != nil {
		t.Fatal(err)
	}
	badJSONResp.Body.Close()
	if badJSONResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 on malformed JSON convert-gitignore, got %d", badJSONResp.StatusCode)
	}

	chunkReq, _ := http.NewRequest(http.MethodPost, wc.base+"/api/v1/doctor", io.LimitReader(strings.NewReader(""), 0))
	chunkReq.ContentLength = -1 // simulates Transfer-Encoding: chunked with 0-byte EOF body
	chunkReq.AddCookie(wc.cookie)
	chunkReq.Header.Set(csrfHeaderName, wc.csrf)
	chunkResp, err := wc.client.Do(chunkReq)
	if err != nil {
		t.Fatal(err)
	}
	chunkResp.Body.Close()
	if chunkResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on empty chunked POST /api/v1/doctor, got %d", chunkResp.StatusCode)
	}

	patAdd := wc.do(http.MethodPost, "/api/v1/memory", map[string]any{
		"kind":         "pattern",
		"pattern_type": "testing",
		"description":  "table-driven tests",
	}, nil)
	patAdd.Body.Close()
	if patAdd.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/memory pattern failed: %d", patAdd.StatusCode)
	}
	memGet := wc.do(http.MethodGet, "/api/v1/memory", nil, nil)
	var memOut struct {
		Patterns []core.Pattern `json:"patterns"`
	}
	_ = json.NewDecoder(memGet.Body).Decode(&memOut)
	memGet.Body.Close()
	if len(memOut.Patterns) == 0 {
		t.Fatal("expected pattern in GET /api/v1/memory")
	}
	patDel := wc.do(http.MethodDelete, "/api/v1/memory?kind=pattern", map[string]any{"id": memOut.Patterns[0].ID}, nil)
	patDel.Body.Close()
	if patDel.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/v1/memory?kind=pattern with JSON id failed: %d", patDel.StatusCode)
	}

	// 6. POST /api/v1/chat/abort cleanly unblocks an active approval.required waiter.
	mGuard := wc.do(http.MethodPatch, "/api/v1/mounts/mode", map[string]any{"path": "blocked.txt", "access_mode": "guarded"}, nil)
	mGuard.Body.Close()
	if mGuard.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /api/v1/mounts/mode guarded failed: %d", mGuard.StatusCode)
	}
	sendResp := wc.do(http.MethodPost, "/api/v1/chat/send", map[string]any{"prompt": "edit blocked.txt"}, nil)
	sendResp.Body.Close()
	if sendResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/v1/chat/send failed: %d", sendResp.StatusCode)
	}
	deadline = time.After(4 * time.Second)
	gotReq := false
	for !gotReq {
		select {
		case msg := <-evCh:
			if msg.event == "approval.required" {
				gotReq = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for approval.required")
		}
	}

	abResp := wc.do(http.MethodPost, "/api/v1/chat/abort", map[string]any{}, nil)
	var abOut struct {
		Aborted bool `json:"aborted"`
	}
	_ = json.NewDecoder(abResp.Body).Decode(&abOut)
	abResp.Body.Close()
	if abResp.StatusCode != http.StatusOK || !abOut.Aborted {
		t.Fatalf("expected abort to cancel active approval turn, status=%d %+v", abResp.StatusCode, abOut)
	}

	deadline = time.After(4 * time.Second)
	gotResolved, gotEnded := false, false
	for !gotResolved || !gotEnded {
		select {
		case msg := <-evCh:
			switch msg.event {
			case "approval.resolved":
				gotResolved = true
			case "chat.done", "chat.error":
				gotEnded = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for abort completion (resolved=%v ended=%v)", gotResolved, gotEnded)
		}
	}
}

func TestConcurrentChatThreadsAndProjectOpenRace(t *testing.T) {
	t.Setenv("TETHER_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	wsA := t.TempDir()
	wsB := t.TempDir()
	wsC := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsA, "hold.txt"), []byte("orig\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mockAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fl, _ := w.(http.Flusher)
			chunks := []string{
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc_hold","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"hold.txt\",\"content\":\"wait\"}"}}]}}]}`,
				`data: {"choices":[{"finish_reason":"tool_calls"}]}`,
				`data: [DONE]`,
			}
			for _, line := range chunks {
				_, _ = fmt.Fprint(w, line+"\n\n")
				if fl != nil {
					fl.Flush()
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id":   "tc_hold",
						"type": "function",
						"function": map[string]any{
							"name":      "write_file",
							"arguments": `{"path":"hold.txt","content":"wait"}`,
						},
					}},
				},
				"finish_reason": "tool_calls",
			}},
		})
	}))
	defer mockAI.Close()

	eng, err := core.NewEngine(wsA)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	runCfg := harness.Config{
		Provider: "deepseek",
		Model:    "deepseek-chat",
		BaseURL:  mockAI.URL,
		APIKey:   "sk-race-test",
		Out:      &bytes.Buffer{},
	}
	run := harness.NewRunner(eng, runCfg)
	defer func() { _ = run.Close() }()

	srv, err := NewServer(Config{Engine: eng, Runner: run, RunnerConfig: runCfg})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		ts.Close()
	}()

	wc := &testWebClient{
		t:    t,
		base: ts.URL,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	wc.redeemBootstrap(srv.BootstrapToken())

	sseResp := wc.do(http.MethodGet, "/api/v1/events", nil, nil)
	defer sseResp.Body.Close()
	evCh := make(chan string, 32)
	go func() {
		sc := bufio.NewScanner(sseResp.Body)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "event: ") {
				evCh <- strings.TrimPrefix(line, "event: ")
			}
		}
	}()

	// Start active turn holding in approval.required.
	mHold := wc.do(http.MethodPatch, "/api/v1/mounts/mode", map[string]any{"path": "hold.txt", "access_mode": "guarded"}, nil)
	mHold.Body.Close()
	if mHold.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /api/v1/mounts/mode hold.txt failed: %d", mHold.StatusCode)
	}
	send1 := wc.do(http.MethodPost, "/api/v1/chat/send", map[string]any{"prompt": "modify hold.txt"}, nil)
	send1.Body.Close()
	if send1.StatusCode != http.StatusAccepted {
		t.Fatalf("first chat/send failed: %d", send1.StatusCode)
	}
	deadline := time.After(4 * time.Second)
	for {
		select {
		case ev := <-evCh:
			if ev == "approval.required" {
				goto approvalActive
			}
		case <-deadline:
			t.Fatal("timed out waiting for approval.required")
		}
	}

approvalActive:
	// Concurrent chat/send & POST /api/v1/threads while turn is active must all get 409 Conflict.
	var conflictWG sync.WaitGroup
	conflicts := make(chan int, 6)
	for i := 0; i < 3; i++ {
		conflictWG.Add(2)
		go func() {
			defer conflictWG.Done()
			r := wc.do(http.MethodPost, "/api/v1/chat/send", map[string]any{"prompt": "concurrent"}, nil)
			r.Body.Close()
			conflicts <- r.StatusCode
		}()
		go func(idx int) {
			defer conflictWG.Done()
			r := wc.do(http.MethodPost, "/api/v1/threads", map[string]any{"title": fmt.Sprintf("Blocked-%d", idx)}, nil)
			r.Body.Close()
			conflicts <- r.StatusCode
		}(i)
	}
	conflictWG.Wait()
	close(conflicts)
	for code := range conflicts {
		if code != http.StatusConflict {
			t.Fatalf("expected 409 Conflict during active chat turn, got %d", code)
		}
	}

	// Concurrent POST /api/v1/project/open while turn is active must serialize cleanly without race/leak.
	var openWG sync.WaitGroup
	openCodes := make(chan int, 2)
	for _, targetWS := range []string{wsB, wsC} {
		openWG.Add(1)
		go func(p string) {
			defer openWG.Done()
			r := wc.do(http.MethodPost, "/api/v1/project/open", map[string]any{"path": p, "init": true}, nil)
			r.Body.Close()
			openCodes <- r.StatusCode
		}(targetWS)
	}
	openWG.Wait()
	close(openCodes)
	for code := range openCodes {
		if code != http.StatusOK {
			t.Fatalf("concurrent POST /api/v1/project/open failed: %d", code)
		}
	}

	// Server stays healthy after project switch and accepts new thread creation.
	thAfter := wc.do(http.MethodPost, "/api/v1/threads", map[string]any{"title": "PostSwitch"}, nil)
	thAfter.Body.Close()
	if thAfter.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/v1/threads after concurrent project/open failed: %d", thAfter.StatusCode)
	}
}
