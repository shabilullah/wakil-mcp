// Package logging holds the small slog helpers shared by the daemon.
package logging

import (
	"context"
	"log/slog"
)

// Quiet returns a logger that drops records below min, keeping everything at or
// above it.
//
// The MCP SDK announces session lifecycle at info level. That is useful for a
// server with real sessions, but this proxy runs session-less: every harness
// request opens and closes a logical session, so the SDK writes three lines per
// tool call and buries the daemon's own events. The SDK therefore gets a
// quieter logger, while its warnings and errors still surface.
func Quiet(log *slog.Logger, min slog.Level) *slog.Logger {
	if log == nil {
		log = slog.Default()
	}
	return slog.New(&minLevelHandler{min: min, next: log.Handler()})
}

// minLevelHandler filters by level and forwards everything else unchanged.
type minLevelHandler struct {
	min  slog.Level
	next slog.Handler
}

func (h *minLevelHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.min && h.next.Enabled(ctx, level)
}

func (h *minLevelHandler) Handle(ctx context.Context, record slog.Record) error {
	// Enabled is a hint, not a contract, so the check is repeated here.
	if record.Level < h.min {
		return nil
	}
	return h.next.Handle(ctx, record)
}

func (h *minLevelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &minLevelHandler{min: h.min, next: h.next.WithAttrs(attrs)}
}

func (h *minLevelHandler) WithGroup(name string) slog.Handler {
	return &minLevelHandler{min: h.min, next: h.next.WithGroup(name)}
}
