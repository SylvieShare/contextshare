package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var oauthScopes = []string{scopeRead, scopeDraft, scopeWork}
var pkceChallenge = regexp.MustCompile("^[A-Za-z0-9_-]{43}$")
var pkceVerifier = regexp.MustCompile("^[A-Za-z0-9._~-]{43,128}$")
var openAICallback = regexp.MustCompile("^/connector/oauth/[A-Za-z0-9_-]{1,150}$")
var localCallback = regexp.MustCompile("^/callback(/[A-Za-z0-9_-]{1,150})?$")

type oauthParams struct {
	ClientID, RedirectURI, State, Scope, Challenge, Resource string
	Draft, Work                                              bool
}

func jsonResponse(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func oauthError(w http.ResponseWriter, code int, name string) {
	jsonResponse(w, code, map[string]string{"error": name})
}

func (a *app) resource() string { return a.cfg.base + "/mcp" }

func (a *app) protectedMetadata(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, 200, map[string]any{
		"resource":                 a.resource(),
		"resource_name":            "ContextShare",
		"authorization_servers":    []string{a.cfg.base},
		"scopes_supported":         oauthScopes,
		"bearer_methods_supported": []string{"header"},
	})
}

func (a *app) authorizationMetadata(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, 200, map[string]any{
		"issuer":                                         a.cfg.base,
		"authorization_endpoint":                         a.cfg.base + "/oauth/authorize",
		"token_endpoint":                                 a.cfg.base + "/oauth/token",
		"registration_endpoint":                          a.cfg.base + "/oauth/register",
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"code_challenge_methods_supported":               []string{"S256"},
		"authorization_response_iss_parameter_supported": true,
		"scopes_supported":                               oauthScopes,
	})
}

func allowedRedirect(raw string) bool {
	if len(raw) > 400 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" || u.RawQuery != "" || u.RawPath != "" {
		return false
	}
	if u.Scheme == "https" && u.Host == "chatgpt.com" {
		return u.Path == "/connector_platform_oauth_redirect" || openAICallback.MatchString(u.Path)
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && ip != nil && ip.IsLoopback() && localCallback.MatchString(u.Path)
}

func registeredRedirect(registered, requested string) bool {
	if !allowedRedirect(requested) {
		return false
	}
	if registered == requested {
		return true
	}
	a, e1 := url.Parse(registered)
	b, e2 := url.Parse(requested)
	return e1 == nil && e2 == nil && a.Scheme == "http" && b.Scheme == "http" && a.Hostname() == b.Hostname() && a.Path == b.Path
}

func (a *app) oauthRegister(w http.ResponseWriter, r *http.Request) {
	if !a.limits.allow("oauth-register:"+a.peer(r), 12) {
		oauthError(w, 429, "slow_down")
		return
	}
	var in struct {
		Redirects []string `json:"redirect_uris"`
		Method    string   `json:"token_endpoint_auth_method"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if json.NewDecoder(r.Body).Decode(&in) != nil || len(in.Redirects) < 1 || len(in.Redirects) > 4 ||
		in.Method != "" && in.Method != "none" {
		oauthError(w, 400, "invalid_client_metadata")
		return
	}
	for _, raw := range in.Redirects {
		if !allowedRedirect(raw) {
			oauthError(w, 400, "invalid_redirect_uri")
			return
		}
	}
	slices.Sort(in.Redirects)
	in.Redirects = slices.Compact(in.Redirects)
	var id string
	err := a.db.pool.QueryRow(r.Context(), a.db.q(`INSERT INTO cs.oauth_clients(id,redirects) VALUES($1,$2)
		ON CONFLICT(redirects) DO UPDATE SET redirects=excluded.redirects RETURNING id`), newID(), in.Redirects).Scan(&id)
	if err != nil {
		oauthError(w, 500, "server_error")
		return
	}
	jsonResponse(w, 201, map[string]any{
		"client_id": id, "redirect_uris": in.Redirects, "token_endpoint_auth_method": "none",
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
	})
}

func (a *app) parseOAuth(r *http.Request, v url.Values) (oauthParams, error) {
	in := oauthParams{
		ClientID: v.Get("client_id"), RedirectURI: v.Get("redirect_uri"), State: v.Get("state"),
		Scope: v.Get("scope"), Challenge: v.Get("code_challenge"), Resource: v.Get("resource"),
	}
	if v.Get("response_type") != "code" || v.Get("code_challenge_method") != "S256" || !pkceChallenge.MatchString(in.Challenge) ||
		len(in.State) > 512 || !textOK(in.ClientID, 1, 80) || (in.Resource != "" && in.Resource != a.resource()) || !allowedRedirect(in.RedirectURI) {
		return in, errInvalid
	}
	var redirects []string
	err := a.db.pool.QueryRow(r.Context(), a.db.q("SELECT redirects FROM cs.oauth_clients WHERE id=$1"), in.ClientID).Scan(&redirects)
	if errors.Is(err, pgx.ErrNoRows) {
		return in, errInvalid
	}
	if err != nil {
		return in, err
	}
	if !slices.ContainsFunc(redirects, func(value string) bool { return registeredRedirect(value, in.RedirectURI) }) {
		return in, errInvalid
	}
	if in.Scope == "" {
		in.Scope = scopeRead + " " + scopeDraft
	}
	for _, scope := range strings.Fields(in.Scope) {
		if !allowed(scope, oauthScopes) {
			return in, errInvalid
		}
		if scope == scopeDraft {
			in.Draft = true
		}
		if scope == scopeWork {
			in.Work = true
		}
	}
	return in, nil
}

func (a *app) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	if r.Method == "POST" {
		if !a.form(w, r) {
			return
		}
		values = r.PostForm
	}
	in, err := a.parseOAuth(r, values)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	u, err := a.session(r)
	if errors.Is(err, errForbidden) && r.Method == "GET" {
		http.Redirect(w, r, "/login?"+url.Values{"next": {r.URL.RequestURI()}}.Encode(), http.StatusSeeOther)
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if r.Method == "GET" {
		projects, err := a.db.projects(r.Context(), u.ID, "")
		if err != nil {
			a.fail(w, r, err)
			return
		}
		// Browsers can enforce form-action after the consent POST redirects.
		// parseOAuth has already matched this callback against the client's registration.
		w.Header().Set("Content-Security-Policy", formSecurityPolicy+" "+in.RedirectURI)
		a.render(w, r, "consent", view{Title: "Разрешить доступ к проекту", User: u, Projects: projects, OAuth: in})
		return
	}
	redirect, _ := url.Parse(in.RedirectURI)
	q := redirect.Query()
	q.Set("iss", a.cfg.base)
	if in.State != "" {
		q.Set("state", in.State)
	}
	if r.PostForm.Get("decision") == "deny" {
		q.Set("error", "access_denied")
		redirect.RawQuery = q.Encode()
		http.Redirect(w, r, redirect.String(), http.StatusSeeOther)
		return
	}
	pid := r.PostForm.Get("project_id")
	role, err := a.db.role(r.Context(), u.ID, pid)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var wanted []string
	if in.Draft && r.PostForm.Get("allow_draft") == "yes" {
		wanted = append(wanted, scopeDraft)
	}
	if in.Work && r.PostForm.Get("allow_work") == "yes" {
		wanted = append(wanted, scopeWork)
	}
	code := randomToken("code_")
	err = inTx(r.Context(), a.db, func(tx pgx.Tx) error {
		g, err := a.db.newGrant(r.Context(), tx, u.ID, pid, "OAuth · ChatGPT / Codex", in.ClientID, allowedScopes(role, wanted))
		if err != nil {
			return err
		}
		_, err = tx.Exec(r.Context(), a.db.q("INSERT INTO cs.oauth_codes(hash,grant_id,redirect_uri,challenge,expires_at) VALUES($1,$2,$3,$4,now()+interval '5 minutes')"), digest(code), g.ID, in.RedirectURI, in.Challenge)
		if err != nil {
			return err
		}
		return a.db.audit(r.Context(), tx, u.ID, pid, "oauth.allow", g.ID)
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	q.Set("code", code)
	redirect.RawQuery = q.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusSeeOther)
}

func (a *app) oauthToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.ParseForm() != nil {
		oauthError(w, 400, "invalid_request")
		return
	}
	if !a.limits.allow("oauth-token:"+a.peer(r), 40) {
		oauthError(w, 429, "slow_down")
		return
	}
	form := r.PostForm
	if v := form.Get("resource"); v != "" && v != a.resource() {
		oauthError(w, 400, "invalid_target")
		return
	}
	clientID := form.Get("client_id")
	if !textOK(clientID, 1, 80) {
		oauthError(w, 400, "invalid_client")
		return
	}
	var access, refresh string
	var g grant
	ctx := r.Context()
	err := inTx(ctx, a.db, func(tx pgx.Tx) error {
		var storedRedirect, challenge string
		var query, credential string
		kind := form.Get("grant_type")
		switch kind {
		case "authorization_code":
			credential = form.Get("code")
			if !strings.HasPrefix(credential, "code_") || !pkceVerifier.MatchString(form.Get("code_verifier")) {
				return errInvalid
			}
			query = `SELECT g.id,g.project_id,g.user_id,g.scopes,g.expires_at,c.redirect_uri,c.challenge
				FROM cs.oauth_codes c JOIN cs.grants g ON c.grant_id=g.id
				JOIN cs.memberships m ON m.project_id=g.project_id AND m.user_id=g.user_id
				WHERE c.hash=$1 AND g.client_id=$2 AND c.expires_at>now() AND g.expires_at>now() AND g.revoked_at IS NULL FOR UPDATE OF c,g`
		case "refresh_token":
			credential = form.Get("refresh_token")
			if !strings.HasPrefix(credential, "refresh_") {
				return errInvalid
			}
			query = `SELECT g.id,g.project_id,g.user_id,g.scopes,g.expires_at,'',''
				FROM cs.tokens t JOIN cs.grants g ON t.grant_id=g.id
				JOIN cs.memberships m ON m.project_id=g.project_id AND m.user_id=g.user_id
				WHERE t.hash=$1 AND t.kind='refresh' AND g.client_id=$2 AND t.expires_at>now()
				AND g.expires_at>now() AND g.revoked_at IS NULL FOR UPDATE OF t,g`
		default:
			return errInvalid
		}
		err := tx.QueryRow(ctx, a.db.q(query), digest(credential), clientID).Scan(&g.ID, &g.ProjectID, &g.UserID, &g.Scopes, &g.Expires, &storedRedirect, &challenge)
		if errors.Is(err, pgx.ErrNoRows) {
			return errInvalid
		}
		if err != nil {
			return err
		}
		if kind == "authorization_code" {
			sum := sha256.Sum256([]byte(form.Get("code_verifier")))
			actual := base64.RawURLEncoding.EncodeToString(sum[:])
			if storedRedirect != form.Get("redirect_uri") || subtle.ConstantTimeCompare([]byte(challenge), []byte(actual)) != 1 {
				return errInvalid
			}
			_, err = tx.Exec(ctx, a.db.q("DELETE FROM cs.oauth_codes WHERE hash=$1"), digest(credential))
		} else {
			if v := form.Get("scope"); v != "" && !slices.Equal(strings.Fields(v), g.Scopes) {
				return errInvalid
			}
			_, err = tx.Exec(ctx, a.db.q("DELETE FROM cs.tokens WHERE hash=$1"), digest(credential))
		}
		if err != nil {
			return err
		}
		access, err = a.db.issueToken(ctx, tx, g, "access", time.Now().Add(time.Hour))
		if err != nil {
			return err
		}
		refresh, err = a.db.issueToken(ctx, tx, g, "refresh", g.Expires)
		return err
	})
	if errors.Is(err, errInvalid) {
		oauthError(w, 400, "invalid_grant")
		return
	}
	if err != nil {
		oauthError(w, 500, "server_error")
		return
	}
	remaining := min(3600, max(0, int(time.Until(g.Expires).Seconds())))
	jsonResponse(w, 200, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": remaining,
		"refresh_token": refresh, "scope": strings.Join(g.Scopes, " "),
	})
}
