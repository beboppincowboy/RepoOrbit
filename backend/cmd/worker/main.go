package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/repoorbit/repoorbit/backend/internal/auth"
	"github.com/repoorbit/repoorbit/backend/internal/db"
	"github.com/repoorbit/repoorbit/backend/internal/githubapp"
	"github.com/repoorbit/repoorbit/backend/internal/ingestion"
	"github.com/repoorbit/repoorbit/backend/internal/jobs"
	"github.com/repoorbit/repoorbit/backend/internal/platform"
	"github.com/repoorbit/repoorbit/backend/internal/repositories"
)

// starRefresher adapts repositories.Service to ingestion.StarRefresher (ingestion cannot import repositories).
// It reads page 1 with refresh=true, which also live-checks page 1's private entries; a dedicated refresh method
// on repositories.Service would avoid that extra GitHub traffic.
type starRefresher struct{ repos *repositories.Service }

func (s starRefresher) RefreshStars(ctx context.Context, userID int64) (ingestion.StarRefresh, error) {
	page, err := s.repos.Starred(ctx, userID, 1, true)
	if errors.Is(err, repositories.ErrSignedOut) {
		return ingestion.StarRefresh{}, ingestion.ErrNoCredential
	}
	if err != nil {
		return ingestion.StarRefresh{}, err
	}
	return ingestion.StarRefresh{Completed: !page.RefreshFailed, RetryAt: page.RetryAt}, nil
}

func main() {
	cfg, err := platform.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	pool, err := platform.OpenDB(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("database unavailable: %v", err)
	}
	defer pool.Close()
	// Demo mode performs no remote ingestion: the worker stays healthy and idle.
	done := make(chan struct{})
	if cfg.Demo {
		close(done)
	} else {
		queries := db.New(pool)
		gh := githubapp.New(cfg.ClientID, cfg.ClientSecret, cfg.GitHubURL, cfg.GitHubAPIURL, cfg.AppURL+"/auth/github/callback")
		authSvc := &auth.Service{Pool: pool, Queries: queries, GitHub: gh, Key: cfg.TokenKey}
		repos := &repositories.Service{Queries: queries, Pool: pool, GitHub: gh, TokenSource: authSvc, AppURL: cfg.AppURL}
		repos.OnStarsReplaced = func(ctx context.Context, tx pgx.Tx, userID int64) error {
			return ingestion.RecheckEligibility(ctx, tx, userID, time.Now())
		}
		clock := jobs.SystemClock{}
		worker := &jobs.Worker{Pool: pool, Clock: clock}
		(&ingestion.Deps{Pool: pool, GitHub: gh, Tokens: authSvc, Access: repos, Clock: clock}).Register(worker)
		ingestion.StartScheduler(ctx, ingestion.SchedulerDeps{Pool: pool, Worker: worker, Stars: starRefresher{repos}, Clock: clock})
		// Pre-U3 selections get durable work too; the queue coalesces repeats.
		if n, err := ingestion.ReconcileSelections(ctx, pool); err != nil {
			log.Printf("startup reconcile failed (will retry via the periodic scheduler): %v", err)
		} else {
			log.Printf("startup reconcile scheduled %d repositories", n)
		}
		go func() {
			defer close(done)
			if err := worker.Run(ctx); err != nil {
				log.Printf("worker stopped: %v", err)
				stop()
			}
		}()
	}
	srv := &http.Server{Addr: ":8081", Handler: platform.Health(pool), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Print(err)
			stop()
		}
	}()
	<-ctx.Done()
	<-done // in-flight jobs finish or are released (30s grace) before exit
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
}
