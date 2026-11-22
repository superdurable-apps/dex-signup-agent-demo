package signuptest

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
)

// LogRecord is one Trigger record: its level, message and attributes as text.
type LogRecord struct {
	Level      slog.Level
	Message    string
	Attributes map[string]string
}

// RecordingLogger keeps every record of its logger, so a test can find a skipped event.
type RecordingLogger struct {
	mutex   sync.Mutex
	records []LogRecord
}

// NewRecordingLogger returns an empty recorder.
func NewRecordingLogger() *RecordingLogger { return &RecordingLogger{} }

// Logger is a logger whose records the recorder keeps.
func (recorder *RecordingLogger) Logger() *slog.Logger {
	return slog.New(&recordingHandler{recorder: recorder})
}

// Find is the records with message whose attribute is value.
func (recorder *RecordingLogger) Find(message, attribute, value string) []LogRecord {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	var found []LogRecord
	for _, record := range recorder.records {
		if record.Message == message && record.Attributes[attribute] == value {
			found = append(found, record)
		}
	}
	return found
}

// Text is every record, one per line, for a failure report.
func (recorder *RecordingLogger) Text() string {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	var builder strings.Builder
	for _, record := range recorder.records {
		fmt.Fprintf(&builder, "%s %s %v\n", record.Level, record.Message, record.Attributes)
	}
	return builder.String()
}

type recordingHandler struct {
	recorder *RecordingLogger
	attrs    []slog.Attr
	groups   []string
}

func (handler *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (handler *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	attributes := map[string]string{}
	prefix := strings.Join(handler.groups, ".")
	add := func(attr slog.Attr) {
		key := attr.Key
		if prefix != "" {
			key = prefix + "." + key
		}
		attributes[key] = attr.Value.Resolve().String()
	}
	for _, attr := range handler.attrs {
		add(attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		add(attr)
		return true
	})
	handler.recorder.mutex.Lock()
	defer handler.recorder.mutex.Unlock()
	handler.recorder.records = append(handler.recorder.records, LogRecord{Level: record.Level, Message: record.Message, Attributes: attributes})
	return nil
}

func (handler *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &recordingHandler{recorder: handler.recorder, attrs: append(append([]slog.Attr(nil), handler.attrs...), attrs...), groups: handler.groups}
}

func (handler *recordingHandler) WithGroup(name string) slog.Handler {
	return &recordingHandler{recorder: handler.recorder, attrs: handler.attrs, groups: append(append([]string(nil), handler.groups...), name)}
}
