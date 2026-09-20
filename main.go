// Command hackathon-webhook receives Azure DevOps pull request webhooks and, when
// the AI Assistant group is on the reviewer list, asks an AI model to rewrite
// the pull request's title and description from its changes and to comment on
// the changed files.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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
	mode          string
	aiBaseURL     string
	aiModel       string
	aiAPIKey      string
	review        bool
	suggestions   bool
}

func loadConfig() (config, error) {
	cfg := config{
		addr:          envOr("LISTEN_ADDR", ":8080"),
		orgURL:        strings.TrimRight(os.Getenv("AZDO_ORG_URL"), "/"),
		pat:           os.Getenv("AZDO_PAT"),
		aiReviewerID:  os.Getenv("AI_REVIEWER_ID"),
		webhookSecret: os.Getenv("WEBHOOK_SECRET"),
		mode:          envOr("ENHANCE_MODE", modeUpdate),
		aiBaseURL:     os.Getenv("AI_BASE_URL"),
		aiModel:       os.Getenv("AI_MODEL"),
		aiAPIKey:      os.Getenv("AI_API_KEY"),
	}
	var missing []string
	for _, required := range [][2]string{
		{"AZDO_ORG_URL", cfg.orgURL},
		{"AZDO_PAT", cfg.pat},
		{"AI_REVIEWER_ID", cfg.aiReviewerID},
		{"AI_BASE_URL", cfg.aiBaseURL},
	} {
		if required[1] == "" {
			missing = append(missing, required[0])
		}
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	review, err := strconv.ParseBool(envOr("REVIEW_COMMENTS", "true"))
	if err != nil {
		return cfg, fmt.Errorf("REVIEW_COMMENTS must be true or false, not %q", os.Getenv("REVIEW_COMMENTS"))
	}
	cfg.review = review
	suggestions, err := strconv.ParseBool(envOr("REVIEW_SUGGESTIONS", "false"))
	if err != nil {
		return cfg, fmt.Errorf("REVIEW_SUGGESTIONS must be true or false, not %q", os.Getenv("REVIEW_SUGGESTIONS"))
	}
	cfg.suggestions = suggestions
	if cfg.mode != modeUpdate && cfg.mode != modeSuggest {
		return cfg, fmt.Errorf("ENHANCE_MODE must be %q or %q, not %q", modeUpdate, modeSuggest, cfg.mode)
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
		log.Print("WARNING: WEBHOOK_SECRET is not set; anyone who can reach /webhook can trigger enhancements")
	}

	ai := newOpenAIClient(cfg.aiBaseURL, cfg.aiModel, cfg.aiAPIKey)
	ai.suggestions = cfg.suggestions
	handler := &webhookHandler{
		aiReviewerID: cfg.aiReviewerID,
		secret:       cfg.webhookSecret,
		mode:         cfg.mode,
		prs:          newAzdoClient(cfg.orgURL, cfg.pat),
		enhancer:     ai,
	}
	if cfg.review {
		handler.reviewer = ai
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

	log.Printf("listening on %s (AI reviewer %s, AI server %s, %s mode, review comments %t, suggestions %t)",
		cfg.addr, cfg.aiReviewerID, cfg.aiBaseURL, cfg.mode, cfg.review, cfg.review && cfg.suggestions)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	log.Print("waiting for in-flight enhancements to finish")
	handler.wait()
}
