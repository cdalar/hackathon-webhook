// Command hackathon-webhook receives Azure DevOps pull request webhooks and, when
// the AI Assistant group is on the reviewer list, asks Claude to review the
// pull request and posts the result back as a PR comment.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

type config struct {
	addr          string
	orgURL        string
	pat           string
	aiReviewerID  string
	webhookSecret string
}

func loadConfig() (config, error) {
	cfg := config{
		addr:          envOr("LISTEN_ADDR", ":8080"),
		orgURL:        strings.TrimRight(os.Getenv("AZDO_ORG_URL"), "/"),
		pat:           os.Getenv("AZDO_PAT"),
		aiReviewerID:  os.Getenv("AI_REVIEWER_ID"),
		webhookSecret: os.Getenv("WEBHOOK_SECRET"),
	}
	var missing []string
	for _, required := range [][2]string{
		{"AZDO_ORG_URL", cfg.orgURL},
		{"AZDO_PAT", cfg.pat},
		{"AI_REVIEWER_ID", cfg.aiReviewerID},
	} {
		if required[1] == "" {
			missing = append(missing, required[0])
		}
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	if cfg.webhookSecret == "" {
		log.Print("WARNING: WEBHOOK_SECRET is not set; anyone who can reach /webhook can trigger reviews")
	}

	handler := &webhookHandler{
		aiReviewerID: cfg.aiReviewerID,
		secret:       cfg.webhookSecret,
		prs:          newAzdoClient(cfg.orgURL, cfg.pat),
		reviewer:     newClaudeReviewer(),
	}

	mux := http.NewServeMux()
	mux.Handle("POST /webhook", handler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{Addr: cfg.addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	log.Printf("listening on %s (AI reviewer %s)", cfg.addr, cfg.aiReviewerID)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	log.Print("waiting for in-flight reviews to finish")
	handler.wait()
}
