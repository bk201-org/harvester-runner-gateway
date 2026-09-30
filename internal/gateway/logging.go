package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/config"
)

type responseLogger struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *responseLogger) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseLogger) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	written, err := w.ResponseWriter.Write(data)
	w.bytes += written
	return written, err
}

// Unwrap lets http.ResponseController reach optional interfaces implemented by
// the underlying writer.
func (w *responseLogger) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func normalizeLogger(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		response := &responseLogger{ResponseWriter: w}
		next.ServeHTTP(response, r)
		if response.status == 0 {
			response.status = http.StatusOK
		}

		level := slog.LevelInfo
		if response.status >= http.StatusInternalServerError {
			level = slog.LevelError
		} else if response.status >= http.StatusBadRequest {
			level = slog.LevelWarn
		}
		s.logger.LogAttrs(r.Context(), level, "request completed",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", response.status),
			slog.Int("response_bytes", response.bytes),
			slog.Int64("duration_ms", time.Since(started).Milliseconds()))
	})
}

func (s *Server) logResourceEvent(ctx context.Context, operation, resourceType, resourceID string,
	owner auth.Owner, policy config.RepositoryPolicy, attributes ...slog.Attr,
) {
	base := []slog.Attr{
		slog.String("operation", operation),
		slog.String("resource_type", resourceType),
		slog.String("resource_id", resourceID),
		slog.String("namespace", policy.Namespace),
		slog.String("repository_id", owner.RepositoryID),
		slog.String("run_id", owner.RunID),
		slog.String("run_attempt", owner.RunAttempt),
	}
	if strings.HasPrefix(owner.RunID, "dev-") {
		base = append(base, slog.String("developer_id", strings.TrimPrefix(owner.RunID, "dev-")))
	}
	s.logger.LogAttrs(ctx, slog.LevelInfo, "resource operation completed", append(base, attributes...)...)
}
