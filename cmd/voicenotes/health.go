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
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/voicenotes/internal/health"
)

const (
	defaultHTTPAddr     = ":8080"
	healthCheckTimeout  = 5 * time.Second
	httpShutdownTimeout = 5 * time.Second
)

// bothLanes is the health view of the two lane workers (research R12): the
// service is alive and its store usable only when every lane is. It never
// looks at vision state (FR-128).
type bothLanes []health.Worker

func (l bothLanes) Alive() bool {
	for _, w := range l {
		if !w.Alive() {
			return false
		}
	}

	return true
}

func (l bothLanes) StoreOK() bool {
	for _, w := range l {
		if !w.StoreOK() {
			return false
		}
	}

	return true
}

// strictness is how a degraded status counts in `voicenotes health`.
type strictness int

const (
	lenient                 strictness = iota // degraded exits 0
	strict                                    // degraded exits 1
	strictIgnoringRecorders                   // degraded exits 1 unless only recorder alerts caused it
)

// runHealth implements `voicenotes health [--strict [--ignore-recorders]]`:
// it GETs the local health endpoint (the distroless image has no curl),
// prints the body and exits 0 on HTTP 200 and 1 otherwise. With --strict,
// degraded also exits 1; --ignore-recorders leaves recorder alerts out of
// that, since the service sends its own message for them.
func runHealth(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(stderr)
	isStrict := fs.Bool("strict", false, "also exit 1 when the status is degraded")
	ignoreRecorders := fs.Bool("ignore-recorders", false,
		"with --strict, do not fail when only microphone recorder alerts make the status degraded")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	mode := lenient
	switch {
	case *ignoreRecorders && !*isStrict:
		_, _ = fmt.Fprintln(stderr, "health: --ignore-recorders needs --strict")
		return 2
	case *ignoreRecorders:
		mode = strictIgnoringRecorders
	case *isStrict:
		mode = strict
	}
	url, err := healthURL(os.Getenv("HTTP_ADDR"))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
	defer cancel()

	return checkHealth(ctx, url, mode, stdout, stderr)
}

// healthURL turns HTTP_ADDR into the loopback URL of the health endpoint.
func healthURL(addr string) (string, error) {
	if addr == "" {
		addr = defaultHTTPAddr
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return "", errors.New("config: HTTP_ADDR: must be host:port or :port")
	}

	return "http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz", nil
}

// checkHealth GETs url, prints the body and returns the process exit code.
func checkHealth(ctx context.Context, url string, mode strictness, stdout, stderr io.Writer) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody) //nolint:gosec // G704: loopback URL.
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "health:", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the URL is loopback plus the configured port.
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
	if mode == lenient {
		return 0
	}
	var rep health.Report
	if err := json.Unmarshal(body, &rep); err != nil {
		return 1
	}
	status := rep.Status
	if mode == strictIgnoringRecorders {
		status = rep.ServiceOnly()
	}
	if status != health.StatusOK {
		return 1
	}

	return 0
}

// listenHealth binds HTTP_ADDR at startup so a bad or busy address fails fast.
func listenHealth(addr string) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("config: HTTP_ADDR: cannot listen: %w", err)
	}

	return ln, nil
}

// uploadReadTimeout bounds reading a whole request. A recording upload may take
// longer: micapi extends its deadlines from its length (extendDeadlines).
const uploadReadTimeout = 2 * time.Minute

// newHTTPServer returns the service's HTTP server. Go starts WriteTimeout when
// the request headers are read, not when the body is done, so it must outlast
// ReadTimeout: ReadTimeout + 30s leaves 30s to answer after the slowest upload.
func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: healthCheckTimeout,
		ReadTimeout:       uploadReadTimeout,
		WriteTimeout:      uploadReadTimeout + 30*time.Second,
	}
}

// serveHTTP serves h on ln until ctx is cancelled, then shuts down gracefully.
func serveHTTP(ctx context.Context, ln net.Listener, h http.Handler, log *zap.Logger) {
	srv := newHTTPServer(h)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("health listener failed", zap.Error(err))
		}
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), httpShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			log.Warn("health listener shutdown", zap.Error(err))
		}
	}
}
