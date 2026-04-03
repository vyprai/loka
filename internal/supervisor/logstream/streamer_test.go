package logstream

import (
	"bufio"
	"encoding/json"
	"net"
	"testing"
	"time"
)

func TestStreamerWriteSingleLine(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()

	s := NewStreamer(client, "service", "svc-1", "stdout")

	done := make(chan struct{})
	var received LogLine
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(server)
		if scanner.Scan() {
			json.Unmarshal(scanner.Bytes(), &received)
		}
	}()

	_, err := s.Write([]byte("hello\n"))
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for log line")
	}

	if received.Message != "hello" {
		t.Fatalf("expected message 'hello', got %q", received.Message)
	}
	if received.Type != "service" {
		t.Fatalf("expected type 'service', got %q", received.Type)
	}
	if received.ID != "svc-1" {
		t.Fatalf("expected id 'svc-1', got %q", received.ID)
	}
	if received.Stream != "stdout" {
		t.Fatalf("expected stream 'stdout', got %q", received.Stream)
	}
}

func TestStreamerWriteMultiline(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()

	s := NewStreamer(client, "task", "t-1", "stderr")

	done := make(chan struct{})
	var lines []LogLine
	go func() {
		defer close(done)
		scanner := bufio.NewScanner(server)
		for scanner.Scan() {
			var ll LogLine
			json.Unmarshal(scanner.Bytes(), &ll)
			lines = append(lines, ll)
			if len(lines) == 2 {
				return
			}
		}
	}()

	_, err := s.Write([]byte("a\nb\n"))
	if err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for log lines")
	}

	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	if lines[0].Message != "a" {
		t.Fatalf("expected 'a', got %q", lines[0].Message)
	}
	if lines[1].Message != "b" {
		t.Fatalf("expected 'b', got %q", lines[1].Message)
	}
}

func TestStreamerWriteAfterClose(t *testing.T) {
	_, client := net.Pipe()
	s := NewStreamer(client, "service", "svc-1", "stdout")
	s.Close()

	// Should not panic.
	n, err := s.Write([]byte("after close\n"))
	if err != nil {
		t.Fatalf("expected no error on write after close, got %v", err)
	}
	if n != len("after close\n") {
		t.Fatalf("expected full length returned, got %d", n)
	}
}

func TestSplitLines(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{"unix newlines", "a\nb\n", []string{"a", "b"}},
		{"windows newlines", "a\r\nb\r\n", []string{"a", "b"}},
		{"trailing text no newline", "a\nb", []string{"a", "b"}},
		{"single line no newline", "hello", []string{"hello"}},
		{"empty string", "", nil},
		{"only newline", "\n", []string{""}},
		{"mixed", "a\r\nb\nc", []string{"a", "b", "c"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitLines(tc.input)
			if len(got) != len(tc.expected) {
				t.Fatalf("expected %d lines, got %d: %v", len(tc.expected), len(got), got)
			}
			for i := range got {
				if got[i] != tc.expected[i] {
					t.Fatalf("line %d: expected %q, got %q", i, tc.expected[i], got[i])
				}
			}
		})
	}
}

func TestStreamerConnectionLost(t *testing.T) {
	server, client := net.Pipe()
	s := NewStreamer(client, "service", "svc-1", "stdout")

	// Close the server side to simulate connection loss.
	server.Close()

	// Write should not block or panic. It should mark as closed internally.
	n, err := s.Write([]byte("data\n"))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if n != len("data\n") {
		t.Fatalf("expected full length, got %d", n)
	}

	// Subsequent writes should still work (silently dropped).
	n, err = s.Write([]byte("more\n"))
	if err != nil {
		t.Fatalf("expected no error on subsequent write, got %v", err)
	}
	if n != len("more\n") {
		t.Fatalf("expected full length, got %d", n)
	}
}
