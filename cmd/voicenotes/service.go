package main

import (
	"context"
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
	"github.com/Toshik1978/voicenotes/internal/digest"
	"github.com/Toshik1978/voicenotes/internal/memos"
	"github.com/Toshik1978/voicenotes/internal/openai"
	"github.com/Toshik1978/voicenotes/internal/pipeline"
	"github.com/Toshik1978/voicenotes/internal/store/sqlite"
	"github.com/Toshik1978/voicenotes/internal/telegram"
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
	// the worker, the purge ticker and, when enabled, the morning digest.
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
	// Before the worker and the poller start, so nothing writes audio yet.
	// Best effort: a failure deletes nothing and never blocks startup.
	if _, serr := pipeline.SweepOrphanAudio(context.Background(), audioDir, db, time.Now(), log); serr != nil {
		log.Warn("orphan audio sweep skipped", zap.Error(serr))
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

	// Later task (LLM enrich): when cfg.LLMEnabled, build the LLM enrich handler here with a
	// plain conditional.
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
	worker.SetQueueNotifier(pipeline.NewQueueNotifier(tg, log))
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

	digestRun, err := digestGoroutines(cfg, log, notes, db, tg)
	if err != nil {
		return nil, err
	}
	goroutines := append([]func(context.Context){
		func(ctx context.Context) { tg.Run(ctx, handler) },
		worker.Run,
		func(ctx context.Context) { pipeline.PurgeLoop(ctx, db, pipeline.PurgeInterval, time.Now, log) },
	}, digestRun...)

	return &service{goroutines: goroutines}, nil
}

// digestGoroutines returns the morning digest runner, or nothing when the
// digest is disabled. T031 (LLM summaries) will call SetSummarizer on the
// runner when cfg.LLMDigestSummary.
func digestGoroutines(
	cfg config.Config, log *zap.Logger, notes digest.NotesReader, store digest.Store, sender digest.Sender,
) ([]func(context.Context), error) {
	if !cfg.DigestEnabled {
		return nil, nil
	}
	runner, err := digest.New(digest.Config{
		ChatID:   cfg.DigestChatID,
		Time:     cfg.DigestTime,
		Loc:      cfg.Location,
		Catchup:  cfg.DigestCatchup,
		MaxNotes: cfg.DigestMaxNotes,
		Log:      log,
	}, notes, store, sender)
	if err != nil {
		return nil, fmt.Errorf("digest: %w", err)
	}

	return []func(context.Context){runner.Run}, nil
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
		InboxTag:  cfg.InboxTag,
		Version:   Version,
	}, nil)

	return transcriber, notes
}
