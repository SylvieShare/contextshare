package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

type user struct{ ID, Email string }
type grant struct {
	ID, Label, ProjectID, UserID, ClientID string
	Scopes                                 []string
	Expires                                time.Time
}

type limiter struct {
	mu      sync.Mutex
	buckets map[string]attempts
}
type attempts struct {
	count int
	until time.Time
}

func (l *limiter) allow(key string, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.buckets == nil {
		l.buckets = map[string]attempts{}
	}
	if len(l.buckets) > 2048 {
		for k, v := range l.buckets {
			if now.After(v.until) {
				delete(l.buckets, k)
			}
		}
		if len(l.buckets) > 4096 {
			return false
		}
	}
	v := l.buckets[key]
	if now.After(v.until) {
		v = attempts{until: now.Add(time.Minute)}
	}
	if v.count >= max {
		return false
	}
	v.count++
	l.buckets[key] = v
	return true
}

func (a *app) peer(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if a.cfg.trustedProxy && net.ParseIP(host).IsLoopback() {
		if ip := net.ParseIP(r.Header.Get("X-Real-IP")); ip != nil {
			return ip.String()
		}
	}
	return host
}

func (a *app) cookieName(name string) string {
	if a.cfg.secure {
		return "__Host-contextshare_" + name
	}
	return "contextshare_" + name
}

func (a *app) cookie(w http.ResponseWriter, name, value string, seconds int) {
	http.SetCookie(w, &http.Cookie{
		Name: a.cookieName(name), Value: value, Path: "/", MaxAge: seconds,
		Secure: a.cfg.secure, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

func (a *app) csrf(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(a.cookieName("csrf")); err == nil && len(c.Value) == 48 {
		return c.Value
	}
	token := randomToken("csrf_")
	a.cookie(w, "csrf", token, 8*60*60)
	return token
}

func (a *app) form(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	if r.ParseForm() != nil {
		http.Error(w, "Некорректная форма", 400)
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != a.cfg.base {
		http.Error(w, "Неверный источник запроса", 403)
		return false
	}
	c, err := r.Cookie(a.cookieName("csrf"))
	if err != nil || len(c.Value) != 48 || subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.PostForm.Get("csrf"))) != 1 {
		http.Error(w, "Сессия формы истекла. Обновите страницу.", 403)
		return false
	}
	return true
}

func (a *app) session(r *http.Request) (user, error) {
	c, err := r.Cookie(a.cookieName("session"))
	if err != nil || len(c.Value) != 48 {
		return user{}, errForbidden
	}
	var u user
	err = a.db.pool.QueryRow(r.Context(), a.db.q("SELECT u.id,u.email FROM cs.sessions s JOIN cs.users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>now()"), digest(c.Value)).Scan(&u.ID, &u.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return user{}, errForbidden
	}
	return u, err
}

func (a *app) signIn(w http.ResponseWriter, r *http.Request, uid string) error {
	token := randomToken("sess_")
	_, err := a.db.pool.Exec(r.Context(), a.db.q("INSERT INTO cs.sessions(token_hash,user_id,expires_at) VALUES($1,$2,now()+interval '7 days')"), digest(token), uid)
	if err != nil {
		return err
	}
	a.cookie(w, "session", token, 7*24*60*60)
	a.cookie(w, "csrf", "", -1)
	return nil
}

func emailAddress(raw string) (string, bool) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if !textOK(value, 3, 254) {
		return "", false
	}
	addr, err := mail.ParseAddress(value)
	return value, err == nil && addr.Address == value && addr.Name == ""
}

func safeReturn(next string) string {
	u, err := url.ParseRequestURI(next)
	if err == nil && u.Host == "" && u.Scheme == "" && u.Path == "/oauth/authorize" && len(next) <= 6000 {
		return next
	}
	return "/"
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		a.render(w, r, "login", view{Title: "Вход", Next: safeReturn(r.URL.Query().Get("next"))})
		return
	}
	if !a.form(w, r) {
		return
	}
	if !a.limits.allow("login:"+a.peer(r), 12) {
		http.Error(w, "Слишком много попыток. Повторите через минуту.", 429)
		return
	}
	email, ok := emailAddress(r.PostForm.Get("email"))
	password := r.PostForm.Get("password")
	var uid string
	var hashed []byte
	var err error
	if ok && len(password) <= 72 {
		err = a.db.pool.QueryRow(r.Context(), a.db.q("SELECT id,password_hash FROM cs.users WHERE email=$1"), email).Scan(&uid, &hashed)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			a.fail(w, r, err)
			return
		}
	}
	if len(hashed) == 0 {
		hashed = a.dummyPassword
	}
	valid := bcrypt.CompareHashAndPassword(hashed, []byte(password)) == nil
	if uid == "" || !valid {
		a.render(w, r, "login", view{Title: "Вход", Next: safeReturn(r.PostForm.Get("next")), Error: "Проверьте почту и пароль."})
		return
	}
	if err := a.signIn(w, r, uid); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, safeReturn(r.PostForm.Get("next")), http.StatusSeeOther)
}

func (a *app) register(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		a.render(w, r, "register", view{Title: "Регистрация"})
		return
	}
	if !a.form(w, r) {
		return
	}
	if !a.limits.allow("register:"+a.peer(r), 5) {
		http.Error(w, "Слишком много попыток. Повторите через минуту.", 429)
		return
	}
	email, ok := emailAddress(r.PostForm.Get("email"))
	password, invite := r.PostForm.Get("password"), strings.TrimSpace(r.PostForm.Get("invite"))
	if !ok || len(password) < 12 || len(password) > 72 || !textOK(invite, 20, 100) {
		a.render(w, r, "register", view{Title: "Регистрация", Error: "Нужны действующая почта, пароль от 12 до 72 байт и код приглашения."})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	uid := newID()
	err = inTx(r.Context(), a.db, func(tx pgx.Tx) error {
		ctx := r.Context()
		var projectID, role string
		if err := tx.QueryRow(ctx, a.db.q("SELECT coalesce(project_id,''),coalesce(role,'') FROM cs.invites WHERE token_hash=$1 AND used_by IS NULL AND expires_at>now() FOR UPDATE"), digest(invite)).Scan(&projectID, &role); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errInvalid
			}
			return err
		}
		if projectID == "" {
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "contextshare-register:"+a.db.schema); err != nil {
				return err
			}
			var exists bool
			if err := tx.QueryRow(ctx, a.db.q("SELECT EXISTS(SELECT 1 FROM cs.users)")).Scan(&exists); err != nil {
				return err
			}
			if exists {
				return errInvalid
			}
		}
		tag, err := tx.Exec(ctx, a.db.q("INSERT INTO cs.users(id,email,password_hash) VALUES($1,$2,$3) ON CONFLICT(email) DO NOTHING"), uid, email, hash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errInvalid
		}
		if projectID != "" {
			if _, err := tx.Exec(ctx, a.db.q("INSERT INTO cs.memberships(project_id,user_id,role) VALUES($1,$2,$3)"), projectID, uid, role); err != nil {
				return err
			}
		} else if err := a.db.applySeed(ctx, tx, uid, a.seed); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, a.db.q("UPDATE cs.invites SET used_by=$2 WHERE token_hash=$1"), digest(invite), uid)
		return err
	})
	if errors.Is(err, errInvalid) {
		a.render(w, r, "register", view{Title: "Регистрация", Error: "Приглашение недействительно или почта уже зарегистрирована."})
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.signIn(w, r, uid); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	if !a.form(w, r) {
		return
	}
	if c, err := r.Cookie(a.cookieName("session")); err == nil {
		if _, err := a.db.pool.Exec(r.Context(), a.db.q("DELETE FROM cs.sessions WHERE token_hash=$1"), digest(c.Value)); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	a.cookie(w, "session", "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *store) tokenGrant(ctx context.Context, raw string) (grant, error) {
	var g grant
	if !strings.HasPrefix(raw, "ctx_") || len(raw) != 47 {
		return g, errForbidden
	}
	err := s.pool.QueryRow(ctx, s.q(`SELECT g.id,g.label,g.project_id,g.user_id,g.client_id,g.scopes,g.expires_at
		FROM cs.tokens t JOIN cs.grants g ON g.id=t.grant_id
		JOIN cs.memberships m ON m.project_id=g.project_id AND m.user_id=g.user_id
		WHERE t.hash=$1 AND t.kind='access' AND t.expires_at>now() AND g.expires_at>now() AND g.revoked_at IS NULL`), digest(raw)).
		Scan(&g.ID, &g.Label, &g.ProjectID, &g.UserID, &g.ClientID, &g.Scopes, &g.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, errForbidden
	}
	return g, err
}

func allowedScopes(role string, wanted []string) []string {
	scopes := []string{scopeRead}
	if role == "owner" || role == "editor" {
		for _, scope := range []string{scopeDraft, scopeWork} {
			if allowed(scope, wanted) {
				scopes = append(scopes, scope)
			}
		}
	}
	return scopes
}

func (s *store) newGrant(ctx context.Context, tx pgx.Tx, uid, pid, label, clientID string, scopes []string) (grant, error) {
	g := grant{ID: newID(), Label: label, ProjectID: pid, UserID: uid, ClientID: clientID, Scopes: scopes, Expires: time.Now().UTC().Add(30 * 24 * time.Hour)}
	_, err := tx.Exec(ctx, s.q("INSERT INTO cs.grants(id,user_id,project_id,label,client_id,scopes,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)"), g.ID, g.UserID, g.ProjectID, g.Label, g.ClientID, g.Scopes, g.Expires)
	return g, err
}

func (s *store) issueToken(ctx context.Context, tx pgx.Tx, g grant, kind string, expiry time.Time) (string, error) {
	prefix := "ctx_"
	if kind == "refresh" {
		prefix = "refresh_"
	}
	raw := randomToken(prefix)
	if expiry.After(g.Expires) {
		expiry = g.Expires
	}
	_, err := tx.Exec(ctx, s.q("INSERT INTO cs.tokens(hash,grant_id,kind,expires_at) VALUES($1,$2,$3,$4)"), digest(raw), g.ID, kind, expiry)
	return raw, err
}
