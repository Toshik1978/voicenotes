package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/capture"
	"github.com/Toshik1978/voicenotes/internal/config"
	"github.com/Toshik1978/voicenotes/internal/memos"
	"github.com/Toshik1978/voicenotes/internal/openai"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
	"github.com/Toshik1978/voicenotes/internal/store/sqlite"
	"github.com/Toshik1978/voicenotes/internal/telegram"
)

const (
	// doneRetention is how long finished captures stay for dedupe
	// (data-model "Purge").
	doneRetention = 30 * 24 * time.Hour
	// purgeInterval is how often done rows are purged (daily).
	purgeInterval = 24 * time.Hour
)

// runService loads the configuration, wires the service and runs it until
// ctx is cancelled. Configuration errors name the variable and never its value.
func runService(ctx context.Context, load func() (config.Config, error), stderr io.Writer) int {
	cfg, err := load()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	log, err := newLogger(cfg.LogLevel)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer func() { _ = log.Sync() }()

	svc, err := build(cfg, log)
	if err != nil {
		log.Error("startup failed", zap.Error(err))
		_, _ = fmt.Fprintln(stderr, "startup failed:", err)

		return 1
	}
	log.Info("voicenotes starting", zap.String("version", Version), zap.String("commit", Commit))
	if err := svc.run(ctx); err != nil {
		log.Error("shutdown failed", zap.Error(err))
		return 1
	}
	log.Info("voicenotes stopped")

	return 0
}

// notifyContext returns a context cancelled by SIGTERM or SIGINT.
func notifyContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
}

func newLogger(level string) (*zap.Logger, error) {
	zc := zap.NewProductionConfig()
	if err := zc.Level.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("config: LOG_LEVEL: invalid value: %w", err)
	}
	log, err := zc.Build()
	if err != nil {
		return nil, fmt.Errorf("build logger: %w", err)
	}

	return log, nil
}

// service is the wired application.
type service struct {
	// goroutines run until ctx is cancelled; they are the Telegram poller,
	// the worker and the purge ticker.
	goroutines []func(context.Context)
	closeStore func() error
}

func (s *service) run(ctx context.Context) error {
	return serve(ctx, s.goroutines, s.closeStore)
}

// serve runs every goroutine until ctx is cancelled, waits for all of them
// to return (Telegram's Run drains its in-flight handlers first), and only
// then closes the store, so no handler or worker step can hit a closed DB.
func serve(ctx context.Context, goroutines []func(context.Context), closeStore func() error) error {
	var wg sync.WaitGroup
	for _, g := range goroutines {
		wg.Go(func() { g(ctx) })
	}
	wg.Wait()
	if err := closeStore(); err != nil {
		return fmt.Errorf("close store: %w", err)
	}

	return nil
}

// build wires the components with plain constructors: data dir, store,
// clients, handlers, worker, Telegram poller, purge ticker.
func build(cfg config.Config, log *zap.Logger) (*service, error) {
	audioDir := filepath.Join(cfg.DataDir, "audio")
	if err := os.MkdirAll(audioDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := sqlite.Open(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	svc, err := wire(cfg, log, db, audioDir)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	svc.closeStore = db.Close

	return svc, nil
}

func wire(cfg config.Config, log *zap.Logger, db *sqlite.DB, audioDir string) (*service, error) {
	transcriber, notes := newClients(cfg)

	tg, err := telegram.NewAPI(telegram.APIConfig{Token: cfg.BotToken, Log: log})
	if err != nil {
		return nil, fmt.Errorf("telegram: %w", err)
	}

	// Later tasks (LLM enrich, morning digest): when cfg.LLMEnabled / cfg.DigestEnabled, build the
	// LLM enrich handler and the morning digest here with plain conditionals.
	handlers, err := pipeline.NewHandlers(pipeline.Deps{
		Transcriber: sttShim{t: transcriber},
		Notes:       notesShim{c: notes},
		Replier:     tg,
		Downloader:  tg,
		Audio:       pipeline.NewDirAudio(audioDir),
		Log:         log,
	}, pipeline.StepsConfig{
		InboxTag:        cfg.InboxTag,
		TodoTag:         cfg.TodoTag,
		NoteLimit:       cfg.MemosContentLimit,
		SourceLanguages: cfg.SourceLanguages,
		Location:        cfg.Location,
	})
	if err != nil {
		return nil, fmt.Errorf("handlers: %w", err)
	}
	worker, err := pipeline.New(db, pipeline.SystemClock{},
		capture.Policy{MaxInterval: cfg.RetryMaxInterval, Rand: rand.Float64}, handlers, log)
	if err != nil {
		return nil, fmt.Errorf("worker: %w", err)
	}
	ingester, err := pipeline.NewIngester(db, worker)
	if err != nil {
		return nil, fmt.Errorf("ingester: %w", err)
	}
	handler, err := telegram.NewHandler(telegram.HandlerConfig{
		AllowedUsers: cfg.AllowedUsers,
		AudioDir:     audioDir,
		Replier:      tg,
		Downloader:   tg,
		Ingester:     ingester,
		Replies:      db,
		Log:          log,
	})
	if err != nil {
		return nil, fmt.Errorf("telegram handler: %w", err)
	}

	return &service{goroutines: []func(context.Context){
		func(ctx context.Context) { tg.Run(ctx, handler) },
		worker.Run,
		func(ctx context.Context) { purgeLoop(ctx, db, purgeInterval, time.Now, log) },
	}}, nil
}

// purger deletes finished captures last updated before a cutoff.
type purger interface {
	PurgeDone(ctx context.Context, olderThan time.Time) (int64, error)
}

// purgeLoop purges done rows older than doneRetention once at start and then
// every interval, until ctx is cancelled. A nil log means no logging.
func purgeLoop(ctx context.Context, p purger, interval time.Duration, now func() time.Time, log *zap.Logger) {
	if log == nil {
		log = zap.NewNop()
	}
	purge := func() {
		n, err := p.PurgeDone(ctx, now().Add(-doneRetention))
		switch {
		case errors.Is(err, context.Canceled):
		case err != nil:
			log.Error("purge failed", zap.Error(err))
		case n > 0:
			log.Info("purged finished captures", zap.Int64("count", n))
		}
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	purge()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			purge()
		}
	}
}

// newClients builds the STT and Memos clients; each http.Client carries its
// configured timeout.
func newClients(cfg config.Config) (*openai.Transcriber, *memos.Client) {
	sttClient := &http.Client{Timeout: cfg.STTTimeout}
	transcriber := openai.NewTranscriber(openai.Config{
		BaseURL:        cfg.STTBaseURL,
		APIKey:         cfg.STTAPIKey,
		Model:          cfg.STTModel,
		ResponseFormat: cfg.STTResponseFormat,
		Version:        Version,
	}, sttClient)
	notes := memos.NewClient(memos.Config{
		BaseURL:   cfg.MemosURL,
		PublicURL: cfg.MemosPublicURL,
		Token:     cfg.MemosToken,
		APIPrefix: cfg.MemosAPIPrefix,
		Version:   Version,
	}, nil)

	return transcriber, notes
}
