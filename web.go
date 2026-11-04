package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

const formSecurityPolicy = "default-src 'none'; style-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

//go:embed web/pages.html
var pagesHTML string

//go:embed web/app.css
var styleCSS string

//go:embed docs/mcp-guide.md
var mcpGuide string

type app struct {
	cfg           config
	db            *store
	seed          *seedBundle
	pages         *template.Template
	dummyPassword []byte
	limits        limiter
	mcp           http.Handler
}

type view struct {
	Title, Error, CSRF, Next, Base, Secret, SecretType string
	User                                               user
	Project                                            project
	Projects                                           []project
	Section                                            section
	Sections                                           []section
	Node                                               node
	Nodes                                              page[node]
	Choices                                            []node
	Related                                            []relatedNode
	Platforms                                          []platformDetail
	Request                                            requestItem
	Requests                                           page[requestItem]
	Counts                                             map[string]int
	Grants                                             []grant
	OAuth                                              oauthParams
	Query, State, Platform, Kind                       string
	Offset, PreviousOffset                             int
	Editable, Owner                                    bool
}

func newApp(cfg config, db *store) (*app, error) {
	seed, err := readSeed(cfg.seedFile)
	if err != nil {
		return nil, errors.New("could not load the pilot import")
	}
	pages, err := template.New("pages").Funcs(template.FuncMap{
		"label":          label,
		"sliceKinds":     func() []string { return nodeKinds },
		"slicePlatforms": func() []string { return platforms },
		"sliceStates":    func() []string { return requestStates },
		"sliceRequests":  func() []string { return requestKinds },
		"formatTime":     func(t time.Time) string { return t.UTC().Format("02.01.2006") },
		"contains":       func(list []string, v string) bool { return allowed(v, list) },
		"hasPart": func(items []workPart, p string) bool {
			for _, i := range items {
				if i.Platform == p {
					return true
				}
			}
			return false
		},
	}).Parse(pagesHTML)
	if err != nil {
		return nil, err
	}
	dummy, err := bcrypt.GenerateFromPassword([]byte(randomToken("")), 12)
	if err != nil {
		return nil, err
	}
	a := &app{cfg: cfg, db: db, seed: seed, pages: pages, dummyPassword: dummy}
	a.mcp = a.makeMCP()
	return a, nil
}

func label(v string) string {
	values := map[string]string{
		"web": "Веб", "android": "Android", "backend": "Сервер",
		"feature": "Фича", "rule": "Правило", "decision": "Решение", "limitation": "Ограничение", "guide": "Инструкция", "evidence": "Проверка",
		"current": "Действует", "planned": "План", "historical": "История", "all": "Все состояния",
		"inbox": "Входящие", "ready": "Готово к работе", "active": "В работе", "done": "Завершено", "archived": "Архив",
		"bug": "Баг", "idea": "Идея", "related": "Связано с", "depends_on": "Зависит от", "verified_by": "Проверяется", "supersedes": "Заменяет",
		"outgoing": "Исходящая", "incoming": "Входящая", "owner": "Владелец", "editor": "Редактор", "viewer": "Читатель",
		scopeRead: "Чтение проекта", scopeDraft: "Добавление заявок", scopeWork: "Взять часть задачи",
	}
	if name, ok := values[v]; ok {
		return name
	}
	return v
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.home)
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /assets/app.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write([]byte(styleCSS))
	})
	for _, method := range []string{"GET", "POST"} {
		mux.HandleFunc(method+" /login", a.login)
		mux.HandleFunc(method+" /register", a.register)
		mux.HandleFunc(method+" /oauth/authorize", a.oauthAuthorize)
	}
	mux.HandleFunc("POST /logout", a.logout)
	mux.HandleFunc("POST /projects", a.createProject)
	mux.HandleFunc("GET /guide", func(w http.ResponseWriter, r *http.Request) {
		a.render(w, r, "guide", view{Title: "Как пользоваться MCP"})
	})
	mux.HandleFunc("GET /guide.md", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		_, _ = w.Write([]byte(mcpGuide))
	})
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", a.protectedMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", a.protectedMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", a.authorizationMetadata)
	mux.HandleFunc("POST /oauth/register", a.oauthRegister)
	mux.HandleFunc("POST /oauth/token", a.oauthToken)
	mux.Handle("/mcp", a.mcp)
	mux.HandleFunc("GET /p/{project}", a.projectPage(false, a.overview))
	mux.HandleFunc("POST /p/{project}/settings", a.projectPage(true, a.updateProject))
	mux.HandleFunc("GET /p/{project}/sections", a.projectPage(false, a.sectionList))
	mux.HandleFunc("POST /p/{project}/sections", a.projectPage(true, a.addSection))
	mux.HandleFunc("GET /p/{project}/sections/{section}", a.projectPage(false, a.sectionPage))
	mux.HandleFunc("POST /p/{project}/sections/{section}/nodes", a.projectPage(true, a.addNode))
	mux.HandleFunc("GET /p/{project}/nodes/{node}", a.projectPage(false, a.nodePage))
	mux.HandleFunc("POST /p/{project}/nodes/{node}", a.projectPage(true, a.updateNode))
	mux.HandleFunc("POST /p/{project}/nodes/{node}/platform", a.projectPage(true, a.updatePlatform))
	mux.HandleFunc("POST /p/{project}/nodes/{node}/links", a.projectPage(true, a.addLink))
	mux.HandleFunc("POST /p/{project}/nodes/{node}/move", a.projectPage(true, a.moveNode))
	mux.HandleFunc("GET /p/{project}/search", a.projectPage(false, a.searchPage))
	mux.HandleFunc("GET /p/{project}/queue", a.projectPage(false, a.queuePage))
	mux.HandleFunc("POST /p/{project}/queue", a.projectPage(true, a.addRequest))
	mux.HandleFunc("GET /p/{project}/queue/{request}", a.projectPage(false, a.requestPage))
	mux.HandleFunc("POST /p/{project}/queue/{request}/prepare", a.projectPage(true, a.prepareRequest))
	mux.HandleFunc("POST /p/{project}/queue/{request}/part", a.projectPage(true, a.changePart))
	mux.HandleFunc("GET /p/{project}/access", a.projectPage(false, a.accessPage))
	mux.HandleFunc("POST /p/{project}/access/tokens", a.projectPage(false, a.createPAT))
	mux.HandleFunc("POST /p/{project}/access/revoke", a.projectPage(false, a.revokeGrant))
	mux.HandleFunc("POST /p/{project}/access/invite", a.projectPage(true, a.inviteMember))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Preserve Origin on same-origin form submissions and suppress cross-origin referrers.
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", formSecurityPolicy)
		if a.cfg.secure {
			w.Header().Set("Strict-Transport-Security", "max-age=86400")
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if a.db.pool.Ping(ctx) != nil {
		jsonResponse(w, 503, map[string]string{"status": "unavailable"})
		return
	}
	jsonResponse(w, 200, map[string]string{"status": "ok", "service": "contextshare", "commitSha": buildCommit})
}

func (a *app) render(w http.ResponseWriter, r *http.Request, name string, v view) {
	v.CSRF = a.csrf(w, r)
	v.Base = a.cfg.base
	if v.Project.ID != "" {
		v.Editable = v.Project.Role == "owner" || v.Project.Role == "editor"
		v.Owner = v.Project.Role == "owner"
	}
	var body bytes.Buffer
	if err := a.pages.ExecuteTemplate(&body, name, v); err != nil {
		log.Printf("template %s failed: %s", name, err)
		http.Error(w, "Не удалось показать страницу", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = body.WriteTo(w)
}

func (a *app) fail(w http.ResponseWriter, r *http.Request, err error) {
	code, title := 500, "Внутренняя ошибка"
	switch {
	case errors.Is(err, errInvalid):
		code, title = 400, "Проверьте параметры"
	case errors.Is(err, errForbidden):
		code, title = 403, "Нет доступа"
	case errors.Is(err, errMissing):
		code, title = 404, "Запись не найдена"
	case errors.Is(err, errConflict):
		code, title = 409, "Запись уже изменилась. Обновите страницу."
	default:
		log.Printf("request failed: %s %s (%T)", r.Method, r.Pattern, err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	a.render(w, r, "error", view{Title: title, Error: title})
}

type projectHandler func(http.ResponseWriter, *http.Request, user, project)

func (a *app) projectPage(edit bool, next projectHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := a.session(r)
		if errors.Is(err, errForbidden) && r.Method == "GET" {
			http.Redirect(w, r, "/login", 303)
			return
		}
		if err != nil {
			a.fail(w, r, err)
			return
		}
		p, err := a.db.project(r.Context(), u.ID, r.PathValue("project"))
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if edit && p.Role == "viewer" {
			a.fail(w, r, errForbidden)
			return
		}
		if r.Method == "POST" && !a.form(w, r) {
			return
		}
		next(w, r, u, p)
	}
}

func ppath(p project, tail string) string { return "/p/" + p.ID + tail }

func (a *app) home(w http.ResponseWriter, r *http.Request) {
	u, err := a.session(r)
	if errors.Is(err, errForbidden) {
		a.render(w, r, "landing", view{Title: "Контекст проекта"})
		return
	}
	if err != nil {
		a.fail(w, r, err)
		return
	}
	ps, err := a.db.projects(r.Context(), u.ID, "")
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.render(w, r, "projects", view{Title: "Проекты", User: u, Projects: ps})
}

func (a *app) createProject(w http.ResponseWriter, r *http.Request) {
	if !a.form(w, r) {
		return
	}
	u, err := a.session(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	p := project{Name: strings.TrimSpace(r.PostForm.Get("name")), Overview: r.PostForm.Get("overview"), RepositoryURL: r.PostForm.Get("repository_url")}
	id, err := a.db.createProject(r.Context(), u.ID, p)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/p/"+id, 303)
}

func (a *app) overview(w http.ResponseWriter, r *http.Request, u user, p project) {
	sections, err := a.db.sections(r.Context(), p.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	counts, err := a.db.queueCounts(r.Context(), p.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.render(w, r, "overview", view{Title: p.Name, Project: p, User: u, Sections: sections, Counts: counts})
}

func (a *app) updateProject(w http.ResponseWriter, r *http.Request, u user, p project) {
	name, overview, repo := strings.TrimSpace(r.PostForm.Get("name")), r.PostForm.Get("overview"), r.PostForm.Get("repository_url")
	if !textOK(name, 1, 160) || !textOK(overview, 0, 6000) || !httpURL(repo) {
		a.fail(w, r, errInvalid)
		return
	}
	err := inTx(r.Context(), a.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), a.db.q("UPDATE cs.projects SET name=$2,overview=$3,repository_url=$4,updated_at=now() WHERE id=$1"), p.ID, name, overview, repo); err != nil {
			return err
		}
		return a.db.audit(r.Context(), tx, u.ID, p.ID, "project.update", p.ID)
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, ppath(p, ""), 303)
}

func (a *app) sectionList(w http.ResponseWriter, r *http.Request, u user, p project) {
	items, err := a.db.sections(r.Context(), p.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.render(w, r, "sections", view{Title: "Разделы", Project: p, User: u, Sections: items})
}

func (a *app) addSection(w http.ResponseWriter, r *http.Request, u user, p project) {
	var id string
	err := inTx(r.Context(), a.db, func(tx pgx.Tx) error {
		var err error
		id, err = a.db.insertSection(r.Context(), tx, p.ID, section{Title: r.PostForm.Get("title"), Summary: r.PostForm.Get("summary")}, 0)
		if err != nil {
			return err
		}
		return a.db.audit(r.Context(), tx, u.ID, p.ID, "section.add", id)
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, ppath(p, "/sections/"+id), 303)
}

func (a *app) sectionPage(w http.ResponseWriter, r *http.Request, u user, p project) {
	v, err := a.db.section(r.Context(), p.ID, r.PathValue("section"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	query := r.URL.Query()
	state := query.Get("status")
	requestedOffset, _ := strconv.Atoi(query.Get("offset"))
	limit, offset := pageArgs(50, requestedOffset)
	nodes, err := a.db.nodes(r.Context(), p.ID, nodeFilter{SectionID: v.ID, Status: state, Limit: limit, Offset: offset})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.render(w, r, "section", view{
		Title: v.Title, Project: p, User: u, Section: v, Nodes: nodes, State: state,
		Offset: offset, PreviousOffset: max(0, offset-limit),
	})
}

func nodeFromForm(r *http.Request) node {
	version, _ := strconv.ParseInt(r.PostForm.Get("version"), 10, 64)
	return node{
		ID: r.PathValue("node"), SectionID: r.PathValue("section"), ParentID: r.PostForm.Get("parent_id"),
		Title: r.PostForm.Get("title"), Kind: r.PostForm.Get("kind"), Body: r.PostForm.Get("body"), Status: r.PostForm.Get("status"),
		SourceURL: r.PostForm.Get("source_url"), SourceRevision: r.PostForm.Get("source_revision"),
		VerifiedAt: r.PostForm.Get("verified_at"), Version: version,
	}
}

func (a *app) addNode(w http.ResponseWriter, r *http.Request, u user, p project) {
	var id string
	err := inTx(r.Context(), a.db, func(tx pgx.Tx) error {
		var err error
		id, err = a.db.insertNode(r.Context(), tx, p.ID, nodeFromForm(r))
		if err != nil {
			return err
		}
		return a.db.audit(r.Context(), tx, u.ID, p.ID, "node.add", id)
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, ppath(p, "/nodes/"+id), 303)
}

func (a *app) nodePage(w http.ResponseWriter, r *http.Request, u user, p project) {
	n, err := a.db.node(r.Context(), p.ID, r.PathValue("node"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	section, err := a.db.section(r.Context(), p.ID, n.SectionID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	related, err := a.db.related(r.Context(), p.ID, n.ID, "", 50, 0)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	platforms, err := a.db.nodePlatforms(r.Context(), p.ID, n.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	choices, err := a.db.nodes(r.Context(), p.ID, nodeFilter{Status: "all", Limit: 50})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	rows, err := a.db.pool.Query(r.Context(), a.db.q("SELECT "+requestPreview+" FROM cs.requests WHERE project_id=$1 AND node_id=$2 ORDER BY created_at DESC LIMIT 30"), p.ID, n.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	requests, err := pgx.CollectRows(rows, requestScan)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	sections, err := a.db.sections(r.Context(), p.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	children, err := a.db.children(r.Context(), p.ID, n.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.render(w, r, "node", view{Title: n.Title, Project: p, User: u, Section: section, Sections: sections, Node: n, Nodes: page[node]{Items: children}, Related: related.Items, Platforms: platforms, Choices: choices.Items, Requests: page[requestItem]{Items: requests}})
}

func (a *app) updateNode(w http.ResponseWriter, r *http.Request, u user, p project) {
	n := nodeFromForm(r)
	err := inTx(r.Context(), a.db, func(tx pgx.Tx) error {
		if err := a.db.editNode(r.Context(), tx, p.ID, n); err != nil {
			return err
		}
		return a.db.audit(r.Context(), tx, u.ID, p.ID, "node.update", n.ID)
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, ppath(p, "/nodes/"+n.ID), 303)
}

func (a *app) updatePlatform(w http.ResponseWriter, r *http.Request, u user, p project) {
	id := r.PathValue("node")
	v := platformDetail{Platform: r.PostForm.Get("platform"), Implementation: r.PostForm.Get("implementation"), Verification: r.PostForm.Get("verification"), Availability: r.PostForm.Get("availability")}
	err := inTx(r.Context(), a.db, func(tx pgx.Tx) error {
		if err := a.db.setPlatform(r.Context(), tx, p.ID, id, v); err != nil {
			return err
		}
		return a.db.audit(r.Context(), tx, u.ID, p.ID, "node.platform."+v.Platform, id)
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, ppath(p, "/nodes/"+id), 303)
}

func (a *app) addLink(w http.ResponseWriter, r *http.Request, u user, p project) {
	id := r.PathValue("node")
	err := inTx(r.Context(), a.db, func(tx pgx.Tx) error {
		if err := a.db.link(r.Context(), tx, p.ID, id, r.PostForm.Get("target_id"), r.PostForm.Get("kind")); err != nil {
			return err
		}
		return a.db.audit(r.Context(), tx, u.ID, p.ID, "node.link", id)
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, ppath(p, "/nodes/"+id), 303)
}

func (a *app) searchPage(w http.ResponseWriter, r *http.Request, u user, p project) {
	q := r.URL.Query()
	state, platform, kind, query := q.Get("status"), q.Get("platform"), q.Get("kind"), strings.TrimSpace(q.Get("q"))
	if state == "" {
		state = "all"
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	nodes, err := a.db.nodes(r.Context(), p.ID, nodeFilter{Query: query, Status: state, Platform: platform, Kind: kind, Offset: offset})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.render(w, r, "search", view{Title: "Поиск", Project: p, User: u, Nodes: nodes, Query: query, State: state, Platform: platform, Kind: kind})
}

func (a *app) queuePage(w http.ResponseWriter, r *http.Request, u user, p project) {
	q := r.URL.Query()
	state, platform, kind := q.Get("state"), q.Get("platform"), q.Get("kind")
	offset, _ := strconv.Atoi(q.Get("offset"))
	items, err := a.db.requests(r.Context(), p.ID, requestFilter{State: state, Platform: platform, Kind: kind, Offset: offset})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	sections, err := a.db.sections(r.Context(), p.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.render(w, r, "queue", view{Title: "Очередь", Project: p, User: u, Requests: items, Sections: sections, State: state, Platform: platform, Kind: kind})
}

func (a *app) addRequest(w http.ResponseWriter, r *http.Request, u user, p project) {
	v := requestInput{Title: r.PostForm.Get("title"), Body: r.PostForm.Get("body"), Kind: r.PostForm.Get("kind"),
		SectionID: r.PostForm.Get("section_id"), NodeID: r.PostForm.Get("node_id"), Platforms: r.PostForm["platforms"]}
	items, err := a.db.addRequests(r.Context(), u.ID, p.ID, newID(), []requestInput{v})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, ppath(p, "/queue/"+items[0].ID), 303)
}

func (a *app) requestPage(w http.ResponseWriter, r *http.Request, u user, p project) {
	item, err := a.db.request(r.Context(), p.ID, r.PathValue("request"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.render(w, r, "request", view{Title: item.Title, Project: p, User: u, Request: item})
}

func (a *app) prepareRequest(w http.ResponseWriter, r *http.Request, u user, p project) {
	id := r.PathValue("request")
	version, _ := strconv.ParseInt(r.PostForm.Get("version"), 10, 64)
	if err := a.db.prepareRequest(r.Context(), u.ID, p.ID, id, r.PostForm.Get("criteria"), version, r.PostForm["platforms"]); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, ppath(p, "/queue/"+id), 303)
}

func (a *app) changePart(w http.ResponseWriter, r *http.Request, u user, p project) {
	id := r.PathValue("request")
	if err := a.db.changePart(r.Context(), u.ID, p.ID, id, r.PostForm.Get("platform"), r.PostForm.Get("action"), r.PostForm.Get("evidence")); err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, ppath(p, "/queue/"+id), 303)
}

func (a *app) accessPage(w http.ResponseWriter, r *http.Request, u user, p project) {
	rows, err := a.db.pool.Query(r.Context(), a.db.q(`SELECT id,label,project_id,user_id,client_id,scopes,expires_at FROM cs.grants
		WHERE project_id=$1 AND user_id=$2 AND revoked_at IS NULL AND expires_at>now() ORDER BY expires_at DESC LIMIT 100`), p.ID, u.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (grant, error) {
		var g grant
		err := row.Scan(&g.ID, &g.Label, &g.ProjectID, &g.UserID, &g.ClientID, &g.Scopes, &g.Expires)
		return g, err
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.render(w, r, "access", view{Title: "Доступ", Project: p, User: u, Grants: items})
}

func (a *app) createPAT(w http.ResponseWriter, r *http.Request, u user, p project) {
	name := strings.TrimSpace(r.PostForm.Get("label"))
	if !textOK(name, 1, 80) {
		a.fail(w, r, errInvalid)
		return
	}
	var raw string
	err := inTx(r.Context(), a.db, func(tx pgx.Tx) error {
		g, err := a.db.newGrant(r.Context(), tx, u.ID, p.ID, name, "", allowedScopes(p.Role, r.PostForm["scopes"]))
		if err != nil {
			return err
		}
		raw, err = a.db.issueToken(r.Context(), tx, g, "access", g.Expires)
		if err != nil {
			return err
		}
		return a.db.audit(r.Context(), tx, u.ID, p.ID, "grant.create", g.ID)
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.render(w, r, "secret", view{Title: "MCP-ключ готов", Project: p, User: u, Secret: raw, SecretType: "token"})
}

func (a *app) revokeGrant(w http.ResponseWriter, r *http.Request, u user, p project) {
	id := r.PostForm.Get("grant_id")
	err := inTx(r.Context(), a.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), a.db.q("UPDATE cs.grants SET revoked_at=now() WHERE project_id=$1 AND user_id=$2 AND id=$3 AND revoked_at IS NULL"), p.ID, u.ID, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errMissing
		}
		return a.db.audit(r.Context(), tx, u.ID, p.ID, "grant.revoke", id)
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	http.Redirect(w, r, ppath(p, "/access"), 303)
}

func (a *app) inviteMember(w http.ResponseWriter, r *http.Request, u user, p project) {
	if p.Role != "owner" {
		a.fail(w, r, errForbidden)
		return
	}
	role := r.PostForm.Get("role")
	if role != "viewer" && role != "editor" {
		a.fail(w, r, errInvalid)
		return
	}
	raw := randomToken("invite_")
	_, err := a.db.pool.Exec(r.Context(), a.db.q("INSERT INTO cs.invites(token_hash,project_id,role,expires_at) VALUES($1,$2,$3,now()+interval '7 days')"), digest(raw), p.ID, role)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.render(w, r, "secret", view{Title: "Приглашение готово", Project: p, User: u, Secret: raw, SecretType: "invite"})
}
