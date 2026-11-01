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
	"github.com/Toshik1978/voicenotes/internal/enrich"
	"github.com/Toshik1978/voicenotes/internal/health"
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

	svc, err := build(cfg, log, time.Now())
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
	// the two lane workers, the purge ticker and, when enabled, the morning digest.
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
func build(cfg config.Config, log *zap.Logger, started time.Time) (*service, error) {
	// Rename a 001 audio/ directory to files/ before anything uses either.
	if err := pipeline.MigrateFilesDir(cfg.DataDir); err != nil {
		return nil, fmt.Errorf("migrate files dir: %w", err)
	}
	filesDir := filepath.Join(cfg.DataDir, "files")
	if err := os.MkdirAll(filesDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := sqlite.Open(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	// Before the worker and the poller start, so nothing writes audio yet.
	// Best effort: a failure deletes nothing and never blocks startup.
	if _, serr := pipeline.SweepOrphanFiles(context.Background(), filesDir, db, time.Now(), log); serr != nil {
		log.Warn("orphan file sweep skipped", zap.Error(serr))
	}
	svc, err := wire(cfg, log, db, filesDir, started)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	svc.closeStore = db.Close

	return svc, nil
}

func wire(
	cfg config.Config, log *zap.Logger, db *sqlite.DB, filesDir string, started time.Time,
) (*service, error) {
	transcriber, notes := newClients(cfg)

	tg, err := telegram.NewAPI(telegram.APIConfig{Token: cfg.BotToken, Log: log})
	if err != nil {
		return nil, fmt.Errorf("telegram: %w", err)
	}

	p, err := newPipeline(cfg, log, db, pipeline.Deps{
		Transcriber: sttShim{t: transcriber},
		Notes:       notesShim{c: notes},
		Replier:     tg,
		Downloader:  tg,
		Files:       pipeline.NewDirFiles(filesDir),
		Log:         log,
	})
	if err != nil {
		return nil, err
	}
	mainWorker, docsWorker, ingester := p.mainWorker, p.docsWorker, p.ingester
	handler, err := telegram.NewHandler(telegram.HandlerConfig{
		AllowedUsers: cfg.AllowedUsers,
		FilesDir:     filesDir,
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
	ln, err := listenHealth(cfg.HTTPAddr)
	if err != nil {
		return nil, err
	}
	collector := health.NewCollector(health.CollectorConfig{
		Queue: healthQueue{db: db}, Worker: bothLanes{mainWorker, docsWorker}, LastPoll: tg.LastPoll,
		Clock: time.Now, Started: started, Threshold: cfg.HealthPendingThreshold,
	})
	mux := http.NewServeMux()
	mux.Handle("/healthz", health.Handler(collector))

	goroutines := append([]func(context.Context){
		func(ctx context.Context) { tg.Run(ctx, handler) },
		mainWorker.Run,
		docsWorker.Run,
		func(ctx context.Context) { serveHTTP(ctx, ln, mux, log) },
		func(ctx context.Context) { pipeline.PurgeLoop(ctx, db, pipeline.PurgeInterval, time.Now, log) },
	}, digestRun...)

	return &service{goroutines: goroutines}, nil
}

// pipelineParts is the queue side of the service: the two lane workers and
// the ingester that feeds them.
type pipelineParts struct {
	mainWorker, docsWorker *pipeline.Worker
	ingester               *pipeline.Ingester
}

// newPipeline builds the step handlers, the main and documents lane workers
// and the ingester, with file ingest enabled. The LLM is wired only when
// configured. Until docling lands the documents lane runs the same handlers;
// it needs only extract.
func newPipeline(cfg config.Config, log *zap.Logger, db *sqlite.DB, deps pipeline.Deps) (*pipelineParts, error) {
	handlers, err := newHandlers(cfg, deps)
	if err != nil {
		return nil, err
	}
	policy := capture.Policy{MaxInterval: cfg.RetryMaxInterval, Rand: rand.Float64}
	notifier := pipeline.NewQueueNotifier(deps.Replier, log)
	mainWorker, err := pipeline.New(capture.LaneMain, db, pipeline.SystemClock{}, policy, handlers, log)
	if err != nil {
		return nil, fmt.Errorf("main worker: %w", err)
	}
	docsWorker, err := pipeline.New(capture.LaneDocuments, db, pipeline.SystemClock{}, policy, handlers, log)
	if err != nil {
		return nil, fmt.Errorf("documents worker: %w", err)
	}
	mainWorker.SetPeers(docsWorker)
	docsWorker.SetPeers(mainWorker)
	mainWorker.SetQueueNotifier(notifier)
	docsWorker.SetQueueNotifier(notifier)

	ingester, err := pipeline.NewIngester(db, wakeAll{mainWorker, docsWorker},
		pipeline.WithFiles(db, cfg.BatchQuietPeriod))
	if err != nil {
		return nil, fmt.Errorf("ingester: %w", err)
	}

	return &pipelineParts{mainWorker: mainWorker, docsWorker: docsWorker, ingester: ingester}, nil
}

// digestGoroutines returns the morning digest runner, or nothing when the
// digest is disabled. With LLM_DIGEST_SUMMARY on and the LLM configured, the
// runner gets an LLM summarizer.
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
	if cfg.LLMEnabled && cfg.LLMDigestSummary {
		runner.SetSummarizer(digest.NewLLMSummarizer(llmShim{c: newChat(cfg)}, cfg.LLMTimeout))
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

// newChat builds the LLM chat client; its http.Client carries LLM_TIMEOUT.
func newChat(cfg config.Config) *openai.Chat {
	return openai.NewChat(openai.ChatConfig{
		BaseURL:     cfg.LLMBaseURL,
		APIKey:      cfg.LLMAPIKey,
		Model:       cfg.LLMModel,
		Version:     Version,
		Temperature: cfg.LLMTemperature,
	}, &http.Client{Timeout: cfg.LLMTimeout})
}

// newHandlers builds the step handlers; the LLM is wired only when configured.
func newHandlers(cfg config.Config, deps pipeline.Deps) (map[capture.Step]pipeline.Handler, error) {
	if cfg.LLMEnabled {
		deps.Enricher = llmShim{c: newChat(cfg)}
	}

	handlers, err := pipeline.NewHandlers(deps, pipeline.StepsConfig{
		InboxTag:        cfg.InboxTag,
		TodoTag:         cfg.TodoTag,
		NoteLimit:       cfg.MemosContentLimit,
		SourceLanguages: cfg.SourceLanguages,
		Location:        cfg.Location,
		Enrich: enrich.Options{
			Translate: cfg.LLMTranslate, Tidy: cfg.LLMTidy, Tasks: cfg.LLMTasks,
			Target: cfg.TargetLanguage, MaxTags: cfg.LLMMaxTags,
			InboxTag: cfg.InboxTag, TodoTag: cfg.TodoTag,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("handlers: %w", err)
	}

	return handlers, nil
}

// wakeAll fans the ingest hooks out to every lane worker.
type wakeAll []pipeline.Waker

func (w wakeAll) Wake() {
	for _, x := range w {
		x.Wake()
	}
}

// HoldAck holds the capture on every lane and releases all of them together.
func (w wakeAll) HoldAck(c capture.Capture) (release func()) {
	releases := make([]func(), len(w))
	for i, x := range w {
		releases[i] = x.HoldAck(c)
	}

	return func() {
		for _, r := range releases {
			r()
		}
	}
}
