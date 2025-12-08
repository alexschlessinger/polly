package log

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var logger *zap.SugaredLogger

// InitLogger initializes the global zap logger
// If debug is true, uses development config with colored console output
// If debug is false, uses a no-op logger (silent)
func InitLogger(debug bool) {
	var l *zap.Logger

	if debug {
		config := zap.NewDevelopmentConfig()
		config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		config.EncoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout("15:04:05.000")
		config.DisableStacktrace = true

		var err error
		l, err = config.Build()
		if err != nil {
			panic(err)
		}
	} else {
		l = zap.NewNop()
	}

	zap.ReplaceGlobals(l)
	zap.RedirectStdLog(l)
	logger = l.Sugar()
}

// GetLogger returns the global sugared logger
func GetLogger() *zap.SugaredLogger {
	if logger == nil {
		InitLogger(false)
	}
	return logger
}
