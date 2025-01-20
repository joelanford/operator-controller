package serverutil

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	"github.com/klauspost/compress/gzhttp"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/log"

	catalogdmetrics "github.com/operator-framework/operator-controller/catalogd/internal/metrics"
	"github.com/operator-framework/operator-controller/catalogd/internal/storage"
	"github.com/operator-framework/operator-controller/catalogd/internal/third_party/server"
)

type CatalogServerConfig struct {
	ExternalAddr string
	CatalogAddr  string
	CertFile     string
	KeyFile      string
	LocalStorage storage.Instance
}

func AddCatalogServerToManager(mgr ctrl.Manager, cfg CatalogServerConfig, tlsFileWatcher *certwatcher.CertWatcher) error {
	listener, err := net.Listen("tcp", cfg.CatalogAddr)
	if err != nil {
		return fmt.Errorf("error creating catalog server listener: %w", err)
	}

	if cfg.CertFile != "" && cfg.KeyFile != "" {
		// Use the passed certificate watcher instead of creating a new one
		config := &tls.Config{
			GetCertificate: tlsFileWatcher.GetCertificate,
			MinVersion:     tls.VersionTLS12,
		}
		listener = tls.NewListener(listener, config)
	}

	shutdownTimeout := 30 * time.Second
	handler := cfg.LocalStorage.StorageServerHandler()
	handler = gzhttp.GzipHandler(handler)
	handler = catalogdmetrics.AddMetricsToHandler(handler)
	handler = newLoggingMiddleware(handler)

	catalogServer := server.Server{
		Kind: "catalogs",
		Server: &http.Server{
			Addr:    cfg.CatalogAddr,
			Handler: handler,
			BaseContext: func(_ net.Listener) context.Context {
				return log.IntoContext(context.Background(), mgr.GetLogger().WithName("http.catalogs"))
			},
			ReadTimeout: 5 * time.Second,
			// TODO: Revert this to 10 seconds if/when the API
			// evolves to have significantly smaller responses
			WriteTimeout: 5 * time.Minute,
		},
		ShutdownTimeout: &shutdownTimeout,
		Listener:        listener,
	}

	err = mgr.Add(&catalogServer)
	if err != nil {
		return fmt.Errorf("error adding catalog server to manager: %w", err)
	}

	return nil
}

func newLoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger := logr.FromContextOrDiscard(r.Context())

		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(lrw, r)

		logger.WithValues(
			"method", r.Method,
			"url", r.URL.String(),
			"status", lrw.statusCode,
			"duration", time.Since(start),
			"remoteAddr", r.RemoteAddr,
		).Info("HTTP request processed")
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *loggingResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}
