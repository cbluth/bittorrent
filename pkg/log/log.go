package log

import (
	"log/slog"
	"os"
	"sync"
)

// Level represents logging verbosity levels
type Level int

const (
	LevelError Level = iota // 0 - Error messages only
	LevelWarn               // 1 - Warnings and errors
	LevelInfo               // 2 - Info, warnings, and errors (default)
	LevelDebug              // 3 - Debug and above
	LevelTrace              // 4 - Maximum verbosity (all messages)
)

// String returns the string representation of the Level
func (l Level) String() string {
	switch l {
	case LevelError:
		return "error"
	case LevelWarn:
		return "warn"
	case LevelInfo:
		return "info"
	case LevelDebug:
		return "debug"
	case LevelTrace:
		return "trace"
	default:
		return "unknown"
	}
}

// ToSlogLevel converts our Level to slog.Level
func (l Level) ToSlogLevel() slog.Level {
	switch l {
	case LevelError:
		return slog.LevelError
	case LevelWarn:
		return slog.LevelWarn
	case LevelInfo:
		return slog.LevelInfo
	case LevelDebug, LevelTrace:
		return slog.LevelDebug // slog doesn't have trace, map to debug
	default:
		return slog.LevelInfo
	}
}

// Mode represents the logging mode
type Mode string

const (
	ModeServer Mode = "server" // JSON structured logging to stdout
	ModeClient Mode = "client" // Human-friendly logging to stderr
	ModeQuiet  Mode = "quiet"  // No logging
)

var (
	mu     sync.RWMutex
	mode   Mode = ModeClient
	logger *slog.Logger

	// Dynamic level — shared across handler rebuilds.
	level slog.LevelVar

	// Active topic filter (nil = no filtering).
	topics []string
)

func init() {
	level.Set(slog.LevelInfo)
	logger = buildLogger(ModeClient)
}

// buildLogger creates a logger for the given mode using the current level and topics.
func buildLogger(m Mode) *slog.Logger {
	var base slog.Handler
	opts := &slog.HandlerOptions{Level: &level}

	switch m {
	case ModeServer:
		base = slog.NewJSONHandler(os.Stdout, opts)
	case ModeQuiet:
		base = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.Level(1000),
		})
	default: // ModeClient
		base = slog.NewTextHandler(os.Stderr, opts)
	}

	handler := newTopicHandler(base, topics)
	return slog.New(handler)
}

// rebuild recreates the logger with current mode/level/topics. Must hold mu.
func rebuild() {
	logger = buildLogger(mode)
}

// SetMode sets the logging mode
func SetMode(m Mode) {
	mu.Lock()
	defer mu.Unlock()
	mode = m
	rebuild()
}

// GetMode returns the current logging mode
func GetMode() Mode {
	mu.RLock()
	defer mu.RUnlock()
	return mode
}

// SetTopics configures per-subsystem log filtering.
// nil/empty or ["all"] = log everything (default).
func SetTopics(t []string) {
	mu.Lock()
	defer mu.Unlock()
	topics = t
	rebuild()
}

// Info logs an informational message
func Info(msg string, args ...any) {
	mu.RLock()
	l := logger
	mu.RUnlock()
	l.Info(msg, args...)
}

// Error logs an error message
func Error(msg string, args ...any) {
	mu.RLock()
	l := logger
	mu.RUnlock()
	l.Error(msg, args...)
}

// Warn logs a warning message
func Warn(msg string, args ...any) {
	mu.RLock()
	l := logger
	mu.RUnlock()
	l.Warn(msg, args...)
}

// Debug logs a debug message (only if level allows)
func Debug(msg string, args ...any) {
	mu.RLock()
	l := logger
	mu.RUnlock()
	l.Debug(msg, args...)
}

// SetLevel sets the minimum logging level using slog.Level
func SetLevel(l slog.Level) {
	level.Set(l)
}

// SetLevelFromInt sets the logging level from an integer verbosity value
// 0=error, 1=warn, 2=info, 3=debug, 4+=trace
func SetLevelFromInt(verbosity int) {
	var l Level
	switch {
	case verbosity <= 0:
		l = LevelError
	case verbosity == 1:
		l = LevelWarn
	case verbosity == 2:
		l = LevelInfo
	case verbosity == 3:
		l = LevelDebug
	default: // 4+
		l = LevelTrace
	}
	SetLevel(l.ToSlogLevel())
}

// SetLevelFromLevel sets the logging level using our Level type
func SetLevelFromLevel(l Level) {
	SetLevel(l.ToSlogLevel())
}

// With returns a new logger with the given attributes
// Useful for adding context to a series of log messages
func With(args ...any) *slog.Logger {
	mu.RLock()
	l := logger
	mu.RUnlock()
	return l.With(args...)
}

// Logger returns the underlying slog.Logger for advanced use cases
func Logger() *slog.Logger {
	mu.RLock()
	l := logger
	mu.RUnlock()
	return l
}
