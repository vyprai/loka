package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/vyprai/loka/internal/loka"
)

// StreamEvent is a single SSE event sent to the client.
type StreamEvent struct {
	Event string `json:"event"` // "output", "status", "result", "error", "approval_required"
	Data  any    `json:"data"`
}

type outputData struct {
	CommandID string `json:"command_id"`
	Stream    string `json:"stream"` // "stdout" or "stderr"
	Text      string `json:"text"`
}

type statusData struct {
	Status string `json:"status"`
}

type approvalData struct {
	CommandID string `json:"command_id"`
	Command   string `json:"command"`
	Reason    string `json:"reason"`
}

// streamExecution streams exec output via Server-Sent Events.
//
// GET /api/v1/sessions/:id/exec/:execId/stream
//
// Events:
//   event: status      — execution status changed (running, pending_approval, etc.)
//   event: output      — stdout/stderr chunk from a command
//   event: approval_required — command suspended, waiting for approve/reject
//   event: result      — final result with exit code
//   event: error       — error occurred
//   event: done        — stream complete
func (s *Server) streamExecution(w http.ResponseWriter, r *http.Request) {
	// Resolve session ID (supports name-based lookup).
	_, _ = s.resolveSessionID(r.Context(), chi.URLParam(r, "id"))
	execID := chi.URLParam(r, "execId")

	// Set SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	ctx := r.Context()

	// Send initial status.
	ex, err := s.sessionManager.GetExecution(ctx, execID)
	if err != nil {
		sendSSE(w, flusher, "error", map[string]string{"message": err.Error()})
		return
	}

	sendSSE(w, flusher, "status", statusData{Status: string(ex.Status)})

	// If already terminal, send result and close.
	if isTerminal(ex.Status) {
		sendResults(w, flusher, ex)
		sendSSE(w, flusher, "done", nil)
		return
	}

	// Poll for updates and stream them.
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	lastStatus := ex.Status
	lastResultCount := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ex, err = s.sessionManager.GetExecution(ctx, execID)
			if err != nil {
				sendSSE(w, flusher, "error", map[string]string{"message": err.Error()})
				return
			}

			// Status change.
			if ex.Status != lastStatus {
				sendSSE(w, flusher, "status", statusData{Status: string(ex.Status)})

				if ex.Status == loka.ExecStatusPendingApproval {
					// Send approval_required event for each command.
					for _, cmd := range ex.Commands {
						sendSSE(w, flusher, "approval_required", approvalData{
							CommandID: cmd.ID,
							Command:   cmd.Command,
							Reason:    "command requires approval",
						})
					}
				}

				lastStatus = ex.Status
			}

			// New results (streaming output as it arrives).
			if len(ex.Results) > lastResultCount {
				for i := lastResultCount; i < len(ex.Results); i++ {
					r := ex.Results[i]
					if r.Stdout != "" {
						sendSSE(w, flusher, "output", outputData{
							CommandID: r.CommandID,
							Stream:    "stdout",
							Text:      r.Stdout,
						})
					}
					if r.Stderr != "" {
						sendSSE(w, flusher, "output", outputData{
							CommandID: r.CommandID,
							Stream:    "stderr",
							Text:      r.Stderr,
						})
					}
				}
				lastResultCount = len(ex.Results)
			}

			// Terminal state — send final results and close.
			if isTerminal(ex.Status) {
				sendResults(w, flusher, ex)
				sendSSE(w, flusher, "done", nil)
				return
			}
		}
	}
}

// execAndStream starts execution and streams output in one request.
//
// POST /api/v1/sessions/:id/exec/stream
//
// Same body as POST /exec, but response is SSE stream instead of JSON.
func (s *Server) execAndStream(w http.ResponseWriter, r *http.Request) {
	var req execReq
	if err := decodeJSON(r, &req); err != nil {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		sendSSE(w, flusher, "error", map[string]string{"message": "invalid request body"})
		return
	}
	sessionID, err := s.resolveSessionID(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		f, _ := w.(http.Flusher)
		sendSSE(w, f, "error", map[string]string{"message": err.Error()})
		return
	}

	var commands []loka.Command
	if req.Command != "" {
		commands = []loka.Command{{
			ID: "cmd-1", Command: req.Command, Args: req.Args,
			Workdir: req.Workdir, Env: req.Env,
		}}
	} else {
		for i, c := range req.Commands {
			id := c.ID
			if id == "" {
				id = fmt.Sprintf("cmd-%d", i+1)
			}
			commands = append(commands, loka.Command{
				ID: id, Command: c.Command, Args: c.Args,
				Workdir: c.Workdir, Env: c.Env,
			})
		}
	}

	if len(commands) == 0 {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		sendSSE(w, flusher, "error", map[string]string{"message": "no commands"})
		return
	}

	ex, err := s.sessionManager.Exec(r.Context(), sessionID, commands, req.Parallel)
	if err != nil {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		sendSSE(w, flusher, "error", map[string]string{"message": err.Error()})
		return
	}

	// Now stream the execution using the same streaming logic.
	r2 := r.Clone(r.Context())
	chi.RouteContext(r2.Context()).URLParams.Add("execId", ex.ID)
	s.streamExecution(w, r2)
}

func sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data any) {
	fmt.Fprintf(w, "event: %s\n", event)
	if data != nil {
		jsonData, _ := json.Marshal(data)
		fmt.Fprintf(w, "data: %s\n", jsonData)
	} else {
		fmt.Fprintf(w, "data: {}\n")
	}
	fmt.Fprintf(w, "\n")
	flusher.Flush()
}

func sendResults(w http.ResponseWriter, flusher http.Flusher, ex *loka.Execution) {
	for _, r := range ex.Results {
		sendSSE(w, flusher, "result", map[string]any{
			"command_id": r.CommandID,
			"exit_code":  r.ExitCode,
			"stdout":     r.Stdout,
			"stderr":     r.Stderr,
		})
	}
}

func isTerminal(status loka.ExecStatus) bool {
	switch status {
	case loka.ExecStatusSuccess, loka.ExecStatusFailed,
		loka.ExecStatusCanceled, loka.ExecStatusRejected:
		return true
	}
	return false
}
