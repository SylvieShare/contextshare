package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var buildCommit = "development"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := configFromEnv()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	setup, stop := context.WithTimeout(ctx, 30*time.Second)
	defer stop()
	db, err := connect(setup, cfg)
	if err != nil {
		return err
	}
	defer db.pool.Close()
	if err := db.migrate(setup, cfg.inviteHash); err != nil {
		return errors.New("database migration failed")
	}
	app, err := newApp(cfg, db)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr: cfg.addr, Handler: app.routes(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 20 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 << 10,
	}
	stopped := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdown, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = srv.Shutdown(shutdown)
		close(stopped)
	}()
	log.Printf("ContextShare %s listening on %s", buildCommit, cfg.addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-stopped
	return nil
}
