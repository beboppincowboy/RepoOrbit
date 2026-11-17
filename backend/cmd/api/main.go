package main

import (
	"context"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	graph "github.com/repoorbit/repoorbit/backend/graphql"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/platform"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

func main() {
	cfg, err := platform.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err = platform.Migrate(ctx, cfg.DatabaseURL); err != nil {
		log.Fatalf("database migration failed: %v", err)
	}
	if cfg.Demo {
		if err = platform.SeedDemo(ctx, cfg.DatabaseURL); err != nil {
			log.Fatalf("demo seed failed: %v", err)
		}
	}
	pool, err := platform.OpenDB(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database unavailable: %v", err)
	}
	defer pool.Close()
	queries := db.New(pool)
	authHandler := &auth.Handler{Demo: cfg.Demo, AppURL: cfg.AppURL}
	resolver := &graph.Resolver{Queries: queries, Demo: cfg.Demo}
	var repoAPI http.Handler
	if !cfg.Demo {
		gh := githubapp.New(cfg.ClientID, cfg.ClientSecret, cfg.GitHubURL, cfg.GitHubAPIURL, cfg.AppURL+"/auth/github/callback")
		authHandler.Service = &auth.Service{Pool: pool, Queries: queries, GitHub: gh, Key: cfg.TokenKey}
		repos := &repositories.Service{Queries: queries, Pool: pool, GitHub: gh, TokenSource: authHandler.Service, AppURL: cfg.AppURL}
		if cfg.AppSlug != "" {
			repos.InstallURL = cfg.GitHubURL + "/apps/" + cfg.AppSlug + "/installations/new"
		}
		authHandler.Installed = http.HandlerFunc(repos.Installed)
		resolver.RepoAccess, repoAPI = repos, authHandler.Protect(repos.Routes())
	}
	router := chi.NewRouter()
	router.Mount("/", platform.Health(pool))
	router.Mount("/auth", authHandler.Routes())
	if repoAPI != nil {
		router.Mount("/api", repoAPI)
	}
	router.Handle("/graphql", authHandler.Protect(graph.Server(resolver)))
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: router, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Print(err)
			stop()
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
}
