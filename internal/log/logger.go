package log

import (
	"log/slog"
	"os"
	"time"

	"github.com/lmittmann/tint"
)

// InitLogger initializes the global slog logger.
// If debug is true, uses tint with colorized console output on stderr, so
// diagnostics never mix into stdout, which carries the program's actual
// output (a piped reply, --schema JSON, or the terminal UI).
// If debug is false, uses a discard-backed logger (silent).
func InitLogger(debug bool) {
	var handler slog.Handler = slog.DiscardHandler
	if debug {
		handler = tint.NewHandler(os.Stderr, &tint.Options{
			Level:      slog.LevelDebug,
			AddSource:  true,
			TimeFormat: time.DateTime,
		})
	}
	slog.SetDefault(slog.New(handler))
}
