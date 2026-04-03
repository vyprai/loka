// Package logging provides a centralized logging system with BadgerDB-backed
// storage, LogQL query language, and Loki-compatible REST API.
package logging

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"
)

// LogEntry is a single log record.
type LogEntry struct {
	Timestamp time.Time         `json:"ts"`
	Level     string            `json:"level"`
	Message   string            `json:"msg"`
	Labels    map[string]string `json:"labels"`
	Fields    map[string]string `json:"fields,omitempty"`
}

// StreamID returns a unique identifier for this log's label combination.
// Uses FNV-1a hash of sorted label key=value pairs (same approach as metrics SeriesID).
func (e LogEntry) StreamID() uint64 {
	h := fnv.New64a()
	keys := make([]string, 0, len(e.Labels))
	for k := range e.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 {
			h.Write([]byte{','})
		}
		h.Write([]byte(k))
		h.Write([]byte{'='})
		h.Write([]byte(e.Labels[k]))
	}
	return h.Sum64()
}

// StreamKey returns the canonical string representation of this log's labels.
func (e LogEntry) StreamKey() string {
	keys := make([]string, 0, len(e.Labels))
	for k := range e.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%q", k, e.Labels[k])
	}
	b.WriteByte('}')
	return b.String()
}

// Standard label keys.
const (
	LabelSource   = "source"    // cp, worker, supervisor, gateway
	LabelType     = "type"      // system, service, task, exec, audit
	LabelID       = "id"        // entity ID
	LabelName     = "name"      // entity name
	LabelWorkerID = "worker_id" // which worker
	LabelLevel    = "level"     // log level
	LabelStream   = "stream"    // stdout, stderr
	LabelAction   = "action"    // for audit logs
)

// Source values.
const (
	SourceCP         = "cp"
	SourceWorker     = "worker"
	SourceSupervisor = "supervisor"
	SourceGateway    = "gateway"
)

// Type values.
const (
	TypeSystem  = "system"
	TypeService = "service"
	TypeTask    = "task"
	TypeExec    = "exec"
	TypeAudit   = "audit"
)

// QueryRequest describes a log query.
type QueryRequest struct {
	Query     string    // LogQL query string
	Start     time.Time // Inclusive start
	End       time.Time // Inclusive end
	Limit     int       // Max entries (default 100)
	Direction string    // "forward" or "backward" (default "backward")
}

// QueryResult holds query results.
type QueryResult struct {
	Streams []Stream
	Stats   QueryStats
}

// Stream is a group of log entries sharing the same labels.
type Stream struct {
	Labels  map[string]string
	Entries []LogEntry
}

// QueryStats holds query execution statistics.
type QueryStats struct {
	EntriesScanned int64
	BytesProcessed int64
	ExecutionMs    int64
}
