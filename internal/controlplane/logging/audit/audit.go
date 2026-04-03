// Package audit provides a structured audit logger for security-relevant operations.
package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/vyprai/loka/internal/controlplane/logging"
	"github.com/vyprai/loka/internal/controlplane/logging/handler"
)

// Logger writes structured audit log entries.
type Logger struct {
	writer handler.LogWriter
}

// New creates an audit Logger. If writer is nil, Log calls are no-ops.
func New(writer handler.LogWriter) *Logger {
	return &Logger{writer: writer}
}

// Log records an audit event. If the writer is nil, this is a no-op.
func (l *Logger) Log(ctx context.Context, action, actor, target, outcome string, details map[string]string) {
	if l.writer == nil {
		return
	}

	entry := logging.LogEntry{
		Timestamp: time.Now(),
		Level:     "info",
		Message:   fmt.Sprintf("%s %s %s: %s", actor, action, target, outcome),
		Labels: map[string]string{
			logging.LabelSource: logging.SourceCP,
			logging.LabelType:   logging.TypeAudit,
			logging.LabelAction: action,
			logging.LabelLevel:  "info",
		},
		Fields: details,
	}

	l.writer.Write(ctx, []logging.LogEntry{entry})
}
