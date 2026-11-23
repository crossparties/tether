package webui

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/crossparties/tether/internal/mount"
)

const (
	sessionCookieName = "tether_session"
	csrfHeaderName    = "X-Tether-Token"
)

type sessionState struct {
	id        string
	csrfToken string
	createdAt time.Time
}

func randomHexToken(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// BootstrapToken returns the current single-use bootstrap token (for printing
// the local login URL or test redemption).
func (s *Server) BootstrapToken() string {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	return s.bootstrapToken
}

// BootstrapURL returns the copyable loopback URL carrying the single-use ?token=.
func (s *Server) BootstrapURL() string {
	addr := s.Addr()
	if addr == "" {
		return ""
	}
	return fmt.Sprintf("http://%s/?token=%s", addr, s.BootstrapToken())
}

func (s *Server) isLocalhostRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	if !s.isAllowedLoopbackHost(r.Host) {
		return false
	}
	if sfs := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); sfs == "cross-site" {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	if strings.EqualFold(origin, "null") {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	if !s.isAllowedLoopbackHost(u.Host) {
		return false
	}
	if r.Host != "" && !strings.EqualFold(u.Host, r.Host) {
		return false
	}
	return true
}

func (s *Server) isAllowedLoopbackHost(hostport string) bool {
	trimmed := strings.TrimSpace(hostport)
	if trimmed == "" {
		return false
	}
	host := trimmed
	port := ""
	if strings.Contains(trimmed, ":") {
		h, p, err := net.SplitHostPort(trimmed)
		if err != nil {
			// Bare IPv6 like "::1" without port.
			if ip := net.ParseIP(strings.Trim(trimmed, "[]")); ip != nil && ip.IsLoopback() {
				s.authMu.Lock()
				bp := s.boundPort
				s.authMu.Unlock()
				return bp == 0
			}
			return false
		}
		host = h
		port = p
	}

	s.authMu.Lock()
	boundPort := s.boundPort
	s.authMu.Unlock()
	if boundPort > 0 && port != strconv.Itoa(boundPort) {
		return false
	}

	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) redeemBootstrapToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.isLocalhostRequest(r) {
		writeError(w, http.StatusForbidden, "forbidden origin or host")
		return
	}

	rawToken := strings.TrimSpace(r.URL.Query().Get("token"))
	if rawToken == "" {
		writeError(w, http.StatusUnauthorized, "bootstrap token required")
		return
	}

	s.authMu.Lock()
	used := s.bootstrapUsed
	expected := s.bootstrapToken
	match := !used && expected != "" && len(rawToken) == len(expected) &&
		subtle.ConstantTimeCompare([]byte(rawToken), []byte(expected)) == 1
	if !match {
		s.authMu.Unlock()
		writeError(w, http.StatusUnauthorized, "invalid or already redeemed bootstrap token")
		return
	}
	sid, err1 := randomHexToken(32)
	csrf, err2 := randomHexToken(32)
	if err1 != nil || err2 != nil {
		s.authMu.Unlock()
		writeError(w, http.StatusInternalServerError, "failed to allocate session")
		return
	}
	s.bootstrapUsed = true
	if s.sessions == nil {
		s.sessions = make(map[string]*sessionState)
	}
	s.sessions[sid] = &sessionState{
		id:        sid,
		csrfToken: csrf,
		createdAt: time.Now().UTC(),
	}
	s.authMu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})

	if acceptsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": true,
			"csrf_token":    csrf,
		})
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func acceptsJSON(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json")
}

func (s *Server) lookupSession(r *http.Request) (*sessionState, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || strings.TrimSpace(c.Value) == "" {
		return nil, false
	}
	val := c.Value

	s.authMu.Lock()
	defer s.authMu.Unlock()
	for id, sess := range s.sessions {
		if len(id) == len(val) && subtle.ConstantTimeCompare([]byte(val), []byte(id)) == 1 {
			return sess, true
		}
	}
	return nil, false
}

func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request, enforceCSRF bool) (*sessionState, bool) {
	if !s.isLocalhostRequest(r) {
		writeError(w, http.StatusForbidden, "forbidden host or origin")
		return nil, false
	}
	sess, ok := s.lookupSession(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return nil, false
	}
	if enforceCSRF && isMutatingMethod(r.Method) {
		tok := strings.TrimSpace(r.Header.Get(csrfHeaderName))
		if tok == "" || len(tok) != len(sess.csrfToken) ||
			subtle.ConstantTimeCompare([]byte(tok), []byte(sess.csrfToken)) != 1 {
			writeError(w, http.StatusForbidden, "missing or invalid X-Tether-Token")
			return nil, false
		}
	}
	return sess, true
}

func isMutatingMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func hasTraversalSegment(raw string) bool {
	decoded := strings.ReplaceAll(strings.ToLower(raw), "%2e", ".")
	decoded = strings.ReplaceAll(decoded, "%2f", "/")
	decoded = strings.ReplaceAll(decoded, "%5c", "/")
	decoded = strings.ReplaceAll(decoded, `\`, "/")
	for seg := range strings.SplitSeq(decoded, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/api/v1/"):
			if _, ok := s.requireAuth(w, r, true); !ok {
				return
			}
			if hasTraversalSegment(p) || hasTraversalSegment(r.URL.EscapedPath()) {
				writeError(w, http.StatusForbidden, "access denied: path traversal rejected")
				return
			}
		case p == "/preview" || strings.HasPrefix(p, "/preview/"):
			if _, ok := s.requireAuth(w, r, false); !ok {
				return
			}
			if hasTraversalSegment(p) || hasTraversalSegment(r.URL.EscapedPath()) {
				writeError(w, http.StatusForbidden, "access denied: path traversal rejected")
				return
			}
		default:
			if !s.isLocalhostRequest(r) {
				writeError(w, http.StatusForbidden, "forbidden host or origin")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.requireAuth(w, r, false)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	eng, run := s.activeState()
	resp := map[string]any{
		"authenticated": true,
		"csrf_token":    sess.csrfToken,
	}
	if eng != nil {
		resp["session_id"] = eng.SessionID
		resp["workspace"] = eng.Workspace
		resp["mode"] = eng.GetMode()
		resp["branch"] = eng.Branch
	}
	if run != nil {
		resp["provider"] = run.ProviderName()
		resp["model"] = run.ModelName()
		resp["thread_id"] = run.ThreadID()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAuth(w, r, false); !ok {
		return
	}

	rawRel := r.PathValue("path")
	if rawRel == "" {
		rawRel = strings.TrimSpace(r.URL.Query().Get("path"))
	}
	if rawRel == "" {
		rawRel = "index.html"
	}

	eng, _ := s.activeState()
	if eng == nil {
		writeError(w, http.StatusServiceUnavailable, "workspace engine unavailable")
		return
	}

	if err := rejectSymlinkSegments(eng.Workspace, rawRel); err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}

	dec := eng.Mount().AuthorizePath(rawRel, mount.OpRead, eng.GetMode())
	if !dec.Allowed {
		writeError(w, http.StatusForbidden, dec.Reason)
		return
	}
	if dec.IsDir {
		idxRel := filepath.ToSlash(filepath.Join(dec.RelPath, "index.html"))
		if err := rejectSymlinkSegments(eng.Workspace, idxRel); err != nil {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		dec = eng.Mount().AuthorizePath(idxRel, mount.OpRead, eng.GetMode())
		if !dec.Allowed {
			writeError(w, http.StatusForbidden, dec.Reason)
			return
		}
	}
	if dec.IsMissing {
		writeError(w, http.StatusNotFound, fmt.Sprintf("file %q not found", dec.RelPath))
		return
	}

	info, err := os.Lstat(dec.CanonicalPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(w, http.StatusNotFound, "file not found")
			return
		}
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		writeError(w, http.StatusForbidden, "refusing non-regular or symlinked preview target")
		return
	}

	const maxPreviewBytes = 4 * 1024 * 1024
	if info.Size() > maxPreviewBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "preview file exceeds 4MB limit")
		return
	}
	data, err := os.ReadFile(dec.CanonicalPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	ctype := mime.TypeByExtension(strings.ToLower(filepath.Ext(dec.CanonicalPath)))
	if ctype == "" {
		ctype = http.DetectContentType(data)
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox allow-scripts; default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self'; connect-src 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'self'")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func rejectSymlinkSegments(workspace, rawPath string) error {
	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		return fmt.Errorf("access denied: empty path")
	}
	relSlash, _, isOutside, err := mount.ResolveWithinWorkspace(trimmed, workspace)
	if err != nil || isOutside || relSlash == "" || relSlash == "." {
		if err != nil {
			return err
		}
		return fmt.Errorf("access denied: path %q escapes workspace", rawPath)
	}

	cleanRel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relSlash)))
	cur := workspace
	for seg := range strings.SplitSeq(cleanRel, "/") {
		if seg == "" || seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		info, lerr := os.Lstat(cur)
		if lerr != nil {
			if os.IsNotExist(lerr) {
				break
			}
			return lerr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("access denied: refusing symlink %q", seg)
		}
	}
	return nil
}
