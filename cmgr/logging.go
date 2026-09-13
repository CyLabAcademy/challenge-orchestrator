package cmgr

import (
	"fmt"
	"log"
	"strings"
)

type LogLevel int

const (
	DISABLED LogLevel = iota
	ERROR
	WARN
	INFO
	DEBUG
)

// LogLevelFromEnv reads CORK_LOGGING, falling back to the level given when
// it is unset or unreadable. Unreadable rather than fatal on purpose: a
// typo in a log level is no reason to refuse to start a daemon, and the
// complaint it returns is logged once the logger exists.
//
// This is the whole of that setting's effect, and for a while it had none:
// the constant was declared and read nowhere, cmgrd hardcoded INFO and
// offered no flag, so there was no way to raise an orchestrator's logging
// at all -- while `cmgrd --help` went on documenting it.
func LogLevelFromEnv(fallback LogLevel) (LogLevel, error) {
	value, isSet := LookupEnv(LOGGING_ENV)
	if !isSet || value == "" {
		return fallback, nil
	}
	switch strings.ToLower(value) {
	case "debug":
		return DEBUG, nil
	case "info":
		return INFO, nil
	case "warn", "warning":
		return WARN, nil
	case "error":
		return ERROR, nil
	case "disabled", "off", "none":
		return DISABLED, nil
	}
	return fallback, fmt.Errorf("%s is '%s', which is not one of debug, info, warn, error or disabled; logging at %s",
		LOGGING_ENV, value, fallback)
}

// String names a level the way CORK_LOGGING spells it.
func (l LogLevel) String() string {
	switch l {
	case DISABLED:
		return "disabled"
	case ERROR:
		return "error"
	case WARN:
		return "warn"
	case INFO:
		return "info"
	case DEBUG:
		return "debug"
	}
	return "unknown"
}

type logger struct {
	logger   *log.Logger
	logLevel LogLevel
}

// Wrapper around the
func newLogger(logLevel LogLevel) *logger {
	l := new(logger)
	l.logger = log.New(log.Writer(), "cmgr: ", log.Flags())
	l.logLevel = logLevel
	return l
}

func (l *logger) debug(v ...interface{}) {
	if l.logLevel >= DEBUG {
		l.logger.Print(append([]interface{}{"DEBUG: "}, v...))
	}
}

func (l *logger) debugf(format string, v ...interface{}) {
	l.debug(fmt.Sprintf(format, v...))
}

func (l *logger) info(v ...interface{}) {
	if l.logLevel >= INFO {
		l.logger.Print(append([]interface{}{"INFO: "}, v...))
	}
}

func (l *logger) infof(format string, v ...interface{}) {
	l.info(fmt.Sprintf(format, v...))
}

func (l *logger) warn(v ...interface{}) {
	if l.logLevel >= WARN {
		l.logger.Print(append([]interface{}{"WARN: "}, v...))
	}
}

func (l *logger) warnf(format string, v ...interface{}) {
	l.warn(fmt.Sprintf(format, v...))
}

func (l *logger) error(v ...interface{}) {
	if l.logLevel >= ERROR {
		l.logger.Print(append([]interface{}{"ERROR: "}, v...))
	}
}

func (l *logger) errorf(format string, v ...interface{}) {
	l.error(fmt.Sprintf(format, v...))
}
