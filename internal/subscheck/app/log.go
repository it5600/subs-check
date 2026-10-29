package app

import (
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"
)

// FileLogger holds the application's own slog output.
var FileLogger = &lumberjack.Logger{
	Filename:   filepath.Join(os.TempDir(), "subs-check.log"),
	MaxSize:    10,
	MaxBackups: 3,
	MaxAge:     7,
}

// GinFileLogger catches gin's access log and panic stacks.
var GinFileLogger = &lumberjack.Logger{
	Filename:   filepath.Join(os.TempDir(), "subs-check-gin.log"),
	MaxSize:    10,
	MaxBackups: 3,
	MaxAge:     7,
}

// TempLog returns the path to the application log.
func TempLog() string {
	return FileLogger.Filename
}
