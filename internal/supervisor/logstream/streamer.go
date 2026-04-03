// Package logstream provides a log push mechanism from supervisor to worker via vsock.
package logstream

import (
	"encoding/json"
	"io"
	"net"
	"sync"
	"time"
)

// LogLine is the wire format for pushed log entries.
type LogLine struct {
	Timestamp time.Time `json:"ts"`
	Level     string    `json:"level"`
	Message   string    `json:"msg"`
	Type      string    `json:"type"`   // service, task, exec
	ID        string    `json:"id"`     // entity ID
	Stream    string    `json:"stream"` // stdout, stderr
}

// Streamer pushes log lines over a network connection (vsock port 54).
// It implements io.Writer so it can be used with io.MultiWriter alongside RingBuffer.
type Streamer struct {
	mu       sync.Mutex
	conn     net.Conn
	encoder  *json.Encoder
	labels   map[string]string // type, id, stream
	closed   bool
}

// NewStreamer creates a log streamer that pushes to the given connection.
func NewStreamer(conn net.Conn, logType, id, stream string) *Streamer {
	return &Streamer{
		conn:    conn,
		encoder: json.NewEncoder(conn),
		labels: map[string]string{
			"type":   logType,
			"id":     id,
			"stream": stream,
		},
	}
}

// Write implements io.Writer. Each write is split into lines and pushed as LogLine entries.
func (s *Streamer) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.conn == nil {
		return len(p), nil // drop silently
	}

	// Split into lines and send each.
	data := string(p)
	lines := splitLines(data)
	for _, line := range lines {
		if line == "" {
			continue
		}
		entry := LogLine{
			Timestamp: time.Now(),
			Level:     "info",
			Message:   line,
			Type:      s.labels["type"],
			ID:        s.labels["id"],
			Stream:    s.labels["stream"],
		}
		if err := s.encoder.Encode(entry); err != nil {
			// Connection lost — mark as closed, don't block the writer.
			s.closed = true
			return len(p), nil
		}
	}
	return len(p), nil
}

// Close closes the streamer.
func (s *Streamer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.conn != nil {
		return s.conn.Close()
	}
	return nil
}

// splitLines splits data into lines, handling \r\n and \n.
func splitLines(data string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			line := data[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			lines = append(lines, line)
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

// NopStreamer returns a writer that discards log lines (used when vsock is unavailable).
func NopStreamer() io.Writer {
	return io.Discard
}
