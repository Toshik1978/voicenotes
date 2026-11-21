package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/config"
	"github.com/Toshik1978/voicenotes/internal/health"
	"github.com/Toshik1978/voicenotes/internal/micaudio"
	"github.com/Toshik1978/voicenotes/internal/micdev"
	"github.com/Toshik1978/voicenotes/internal/recorder"
)

const (
	defaultHTTPAddr     = "127.0.0.1:8081"
	healthCheckTimeout  = 5 * time.Second
	httpShutdownTimeout = 5 * time.Second
)

// runRecorder parses the configuration from environ, wires the recorder and
// runs it with /healthz on MIC_HTTP_ADDR until ctx ends. On cancellation a
// recording in progress is finished as interrupted.
func runRecorder(ctx context.Context, environ []string, stderr io.Writer) int {
	cfg, err := config.ParseRecorder(environ)
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

	ln, err := listen(cfg.HTTPAddr)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}

	rec, err := newRecorder(cfg, log)
	if err != nil {
		_ = ln.Close()
		_, _ = fmt.Fprintln(stderr, err)

		return 1
	}

	log.Info("voicenotes-mic starting",
		zap.String("version", Version), zap.String("commit", Commit), zap.String("recorder", cfg.RecorderName))

	return serve(ctx, ln, rec, cfg.PendingThreshold, log)
}

// newRecorder wires the recorder from its configuration: the spool, the HTTP
// client (hand-off, notices and check-ins), device discovery (auto, or the
// fixed pair when both MIC_CAPTURE_DEVICE and MIC_INPUT_DEVICE are set), the
// key reader and the cgo audio capture.
func newRecorder(cfg config.RecorderConfig, log *zap.Logger) (*recorder.Recorder, error) {
	spool, err := recorder.NewDirSpool(cfg.SpoolDir, nil)
	if err != nil {
		return nil, fmt.Errorf("config: MIC_SPOOL_DIR: %w", err)
	}

	client, err := recorder.NewClient(cfg.ServerURL, cfg.UploadToken, cfg.RecorderName, nil)
	if err != nil {
		return nil, fmt.Errorf("recorder client: %w", err)
	}

	var discovery recorder.Discovery = micdev.Discover{Sys: micdev.HostSysfs()}
	if cfg.CaptureDevice != "" && cfg.InputDevice != "" {
		discovery = micdev.Fixed{CaptureDevice: cfg.CaptureDevice, InputPath: cfg.InputDevice}
	}

	rec, err := recorder.New(recorder.Deps{
		Keys:      micdev.Keys{},
		Audio:     micaudio.New(),
		Discovery: discovery,
		Spool:     spool,
		Handoff:   client,
		Notifier:  client,
		CheckIn:   client,
		Log:       log,
	}, recorder.Options{
		Recorder:         cfg.RecorderName,
		Debounce:         cfg.Debounce,
		MinLength:        cfg.MinLength,
		MaxLength:        cfg.MaxLength,
		CheckinInterval:  cfg.CheckinInterval,
		RetryMaxInterval: cfg.RetryMaxInterval,
		SpoolMinFree:     cfg.SpoolMinFree,
	})
	if err != nil {
		return nil, fmt.Errorf("recorder: %w", err)
	}

	return rec, nil
}

// serve runs the recorder and serves its /healthz on ln until ctx ends, then
// shuts the listener down once the recorder has stopped.
func serve(ctx context.Context, ln net.Listener, rec *recorder.Recorder, threshold time.Duration, log *zap.Logger) int {
	mux := http.NewServeMux()
	mux.Handle("/healthz", recorder.HealthHandler(rec.Status, time.Now, threshold))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: healthCheckTimeout,
		ReadTimeout:       healthCheckTimeout,
		WriteTimeout:      healthCheckTimeout,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	code := 0
	if err := rec.Run(ctx); err != nil {
		log.Error("recorder failed", zap.Error(err))

		code = 1
	}

	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), httpShutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("health listener shutdown", zap.Error(err))
	}

	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		log.Error("health listener failed", zap.Error(err))
	}

	log.Info("voicenotes-mic stopped")

	return code
}

// listen binds MIC_HTTP_ADDR at startup so a bad or busy address fails fast.
func listen(addr string) (net.Listener, error) {
	var lc net.ListenConfig

	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("config: MIC_HTTP_ADDR: cannot listen: %w", err)
	}

	return ln, nil
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

// runHealth implements `voicenotes-mic health [--strict]`: it GETs the
// recorder's /healthz on MIC_HTTP_ADDR, prints the body and exits 0 on HTTP
// 200 and 1 otherwise. With --strict, degraded also exits 1.
func runHealth(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	strict := fs.Bool("strict", false, "also exit 1 when the status is degraded")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	url, err := healthURL(os.Getenv("MIC_HTTP_ADDR"))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
	defer cancel()

	return checkHealth(ctx, url, *strict, stdout, stderr)
}

// healthURL turns MIC_HTTP_ADDR into the URL of the health endpoint. An empty
// or unspecified host (":8081", "0.0.0.0:8081") is reached over loopback.
func healthURL(addr string) (string, error) {
	if addr == "" {
		addr = defaultHTTPAddr
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "", errors.New("config: MIC_HTTP_ADDR: must be host:port or :port")
	}

	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}

	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}

// checkHealth GETs url, prints the body and returns the process exit code.
func checkHealth(ctx context.Context, url string, strict bool, stdout, stderr io.Writer) int {
	// The URL is built from the operator's own MIC_HTTP_ADDR.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody) //nolint:gosec // G704: see above.
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "health:", err)
		return 1
	}

	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the URL is built from MIC_HTTP_ADDR.
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "health:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "health: read body:", err)
		return 1
	}

	_, _ = fmt.Fprintln(stdout, string(body))

	if resp.StatusCode != http.StatusOK {
		return 1
	}

	if strict {
		var rep struct {
			Status health.Status `json:"status"`
		}
		if err := json.Unmarshal(body, &rep); err != nil || rep.Status != health.StatusOK {
			return 1
		}
	}

	return 0
}
