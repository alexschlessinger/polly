package log

import (
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/lmittmann/tint"
)

var logger *slog.Logger

// InitLogger initializes the global slog logger.
// If debug is true, uses tint with colorized console output.
// If debug is false, uses a discard-backed logger (silent).
func InitLogger(debug bool) {
	initLogger(debug, os.Stdout)
}

func initLogger(debug bool, w io.Writer) {
	if w == nil {
		w = os.Stdout
	}

	logger = slog.New(newHandler(debug, w))
	slog.SetDefault(logger)
}

func newHandler(debug bool, w io.Writer) slog.Handler {
	if debug {
		return tint.NewHandler(w, &tint.Options{
			Level:      slog.LevelDebug,
			AddSource:  true,
			TimeFormat: time.DateTime,
		})
	}

	return slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
}

// GetLogger returns the global logger.
func GetLogger() *slog.Logger {
	if logger == nil {
		InitLogger(false)
	}
	return logger
}
