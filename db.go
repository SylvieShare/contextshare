package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

type config struct {
	addr, base, schema, databaseURL, databaseUser, databasePassword string
	caFile, seedFile                                                string
	inviteHash                                                      []byte
	secure, publicDNS, trustedProxy                                 bool
}

var schemaName = regexp.MustCompile("^[a-z][a-z0-9_]{0,47}$")

func configFromEnv() (config, error) {
	get := func(key, fallback string) string {
		if value := os.Getenv(key); value != "" {
			return value
		}
		return fallback
	}
	cfg := config{
		addr:        get("CONTEXTSHARE_ADDR", "127.0.0.1:8080"),
		base:        strings.TrimRight(get("CONTEXTSHARE_BASE_URL", "http://127.0.0.1:8080"), "/"),
		schema:      get("CONTEXTSHARE_DB_SCHEMA", "contextshare"),
		databaseURL: os.Getenv("DB_URL"), databaseUser: os.Getenv("DB_USER"), databasePassword: os.Getenv("DB_PASSWORD"),
		caFile: os.Getenv("CONTEXTSHARE_DB_CA"), seedFile: os.Getenv("CONTEXTSHARE_SEED_FILE"),
		publicDNS:    os.Getenv("CONTEXTSHARE_DB_PUBLIC") == "1",
		trustedProxy: os.Getenv("CONTEXTSHARE_TRUST_LOOPBACK_PROXY") == "1",
	}
	u, err := url.Parse(cfg.base)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return cfg, errors.New("invalid CONTEXTSHARE_BASE_URL")
	}
	cfg.secure = u.Scheme == "https"
	if !cfg.secure && (u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost")) {
		return cfg, errors.New("HTTPS is required outside local development")
	}
	if !schemaName.MatchString(cfg.schema) || cfg.databaseURL == "" {
		return cfg, errors.New("a PostgreSQL URL and valid database schema are required")
	}
	if value := os.Getenv("CONTEXTSHARE_INVITE_SHA256"); value != "" {
		cfg.inviteHash, err = hex.DecodeString(value)
		if err != nil || len(cfg.inviteHash) != sha256.Size {
			return cfg, errors.New("invalid invite hash")
		}
	}
	return cfg, nil
}

type store struct {
	pool   *pgxpool.Pool
	schema string
}

func (s *store) q(query string) string {
	return strings.ReplaceAll(query, "cs.", pgx.Identifier{s.schema}.Sanitize()+".")
}

func connect(ctx context.Context, cfg config) (*store, error) {
	raw := strings.TrimPrefix(cfg.databaseURL, "jdbc:")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() == "" {
		return nil, errors.New("invalid PostgreSQL URL")
	}
	if cfg.publicDNS {
		if !strings.HasSuffix(u.Hostname(), ".mdb.yandexcloud.net") || cfg.caFile == "" {
			return nil, errors.New("public Yandex PostgreSQL requires its host name and CA certificate")
		}
		q := u.Query()
		q.Set("sslmode", "verify-full")
		q.Set("sslrootcert", cfg.caFile)
		u.RawQuery = q.Encode()
	}
	pc, err := pgxpool.ParseConfig(u.String())
	if err != nil {
		return nil, errors.New("invalid PostgreSQL settings")
	}
	if cfg.databaseUser != "" {
		pc.ConnConfig.User = cfg.databaseUser
	}
	if cfg.databasePassword != "" {
		pc.ConnConfig.Password = cfg.databasePassword
	}
	pc.MaxConns = 8
	pc.ConnConfig.ConnectTimeout = 12 * time.Second
	pc.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec
	if cfg.publicDNS {
		pc.ConnConfig.LookupFunc = lookupPublicDB
	}
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, errors.New("could not configure PostgreSQL pool")
	}
	err = pool.Ping(ctx)
	if err != nil && !postgresRejected(err) && ctx.Err() == nil {
		err = pool.Ping(ctx)
	}
	if err != nil {
		pool.Close()
		var serverError *pgconn.PgError
		if errors.As(err, &serverError) {
			return nil, errors.New("PostgreSQL readiness check failed (SQLSTATE " + serverError.Code + ")")
		}
		if errors.Is(err, errPublicDNS) {
			return nil, errors.New("PostgreSQL readiness check failed (public DNS unavailable)")
		}
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return nil, errors.New("PostgreSQL readiness check failed (network timeout)")
		}
		return nil, errors.New("PostgreSQL readiness check failed")
	}
	return &store{pool: pool, schema: cfg.schema}, nil
}

func postgresRejected(err error) bool {
	var serverError *pgconn.PgError
	return errors.As(err, &serverError)
}

var errPublicDNS = errors.New("public database DNS is unavailable")
var publicDNSClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{Transport: transport, Timeout: 4 * time.Second}
}()

func lookupPublicDB(ctx context.Context, host string) ([]string, error) {
	if !strings.HasSuffix(host, ".mdb.yandexcloud.net") {
		return nil, errors.New("unexpected managed database host")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "https://dns.google/resolve?"+url.Values{"name": {host}, "type": {"A"}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := publicDNSClient.Do(req)
	if err != nil {
		resp, err = publicDNSClient.Do(req)
	}
	if err != nil {
		return nil, errPublicDNS
	}
	defer resp.Body.Close()
	var result struct {
		Answer []struct {
			Type int
			Data string
		}
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&result) != nil {
		return nil, errors.New("invalid public database DNS response")
	}
	var addresses []string
	for _, answer := range result.Answer {
		ip := net.ParseIP(answer.Data)
		if answer.Type == 1 && ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() {
			addresses = append(addresses, ip.String())
		}
	}
	if len(addresses) == 0 {
		return nil, errors.New("no public IPv4 for PostgreSQL")
	}
	return addresses, nil
}

func (s *store) migrate(ctx context.Context, inviteHash []byte) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "contextshare:"+s.schema); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{s.schema}.Sanitize()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, s.q(schemaSQL)); err != nil {
		return err
	}
	if len(inviteHash) > 0 {
		if _, err := tx.Exec(ctx, s.q("INSERT INTO cs.invites(token_hash,expires_at) SELECT $1,now()+interval '7 days' WHERE NOT EXISTS (SELECT 1 FROM cs.users) ON CONFLICT DO NOTHING"), inviteHash); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
