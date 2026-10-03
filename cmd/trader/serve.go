package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"agentic-trader/internal/web"
)

// runServe serves the local dashboard until interrupted.
func runServe(ctx context.Context, app *app, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "listen address (keep it on localhost: there is no auth)")
	fs.Parse(args)
	if err := app.requireInitialized(ctx); err != nil {
		return err
	}
	if host, _, err := net.SplitHostPort(*addr); err == nil {
		if ip := net.ParseIP(host); host == "" || ip != nil && !ip.IsLoopback() {
			log.Printf("warning: %s is reachable from other machines and the dashboard has no auth", *addr)
		}
	}

	repo, err := filepath.Abs(filepath.Dir(app.dbPath))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(repo, "logs"), 0o755); err != nil {
		return err
	}
	srv := web.New(web.Config{
		Store: app.store, Tokens: app.tokens, Venue: app.jupiter(), Symbols: app.symbols(),
		Reviewer: &web.ScriptReviewer{
			Script:  filepath.Join(repo, "scripts", "run.sh"),
			Dir:     repo,
			Lock:    filepath.Join(repo, "logs", "agent.lock"),
			LogPath: filepath.Join(repo, "logs", "review-requests.log"),
		},
	})

	hs := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sctx)
	}()
	log.Printf("dashboard on http://%s", *addr)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
