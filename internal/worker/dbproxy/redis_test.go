package dbproxy

import (
	"bufio"
	"strings"
	"testing"
)

func TestReadRESPResponse_SimpleString(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("+OK\r\n"))
	resp, err := readRESPResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp) != "+OK\r\n" {
		t.Errorf("expected +OK, got %q", resp)
	}
}

func TestReadRESPResponse_Error(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("-ERR unknown\r\n"))
	resp, err := readRESPResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(resp), "-ERR") {
		t.Errorf("expected error response, got %q", resp)
	}
}

func TestReadRESPResponse_Integer(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(":42\r\n"))
	resp, err := readRESPResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp) != ":42\r\n" {
		t.Errorf("expected :42, got %q", resp)
	}
}

func TestReadRESPResponse_BulkString(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("$5\r\nhello\r\n"))
	resp, err := readRESPResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp), "hello") {
		t.Errorf("expected bulk string 'hello', got %q", resp)
	}
}

func TestReadRESPResponse_NullBulkString(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("$-1\r\n"))
	resp, err := readRESPResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp) != "$-1\r\n" {
		t.Errorf("expected null bulk, got %q", resp)
	}
}

func TestReadRESPResponse_NullArray(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("*-1\r\n"))
	resp, err := readRESPResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp) != "*-1\r\n" {
		t.Errorf("expected null array, got %q", resp)
	}
}

func TestReadRESPResponse_Array(t *testing.T) {
	input := "*2\r\n$3\r\nfoo\r\n$3\r\nbar\r\n"
	r := bufio.NewReader(strings.NewReader(input))
	resp, err := readRESPResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp), "foo") || !strings.Contains(string(resp), "bar") {
		t.Errorf("expected foo and bar in array, got %q", resp)
	}
}

func TestReadRESPResponse_ArrayCountExceedsMax(t *testing.T) {
	// Array with count > maxRESPArrayCount should error.
	input := "*999999\r\n"
	r := bufio.NewReader(strings.NewReader(input))
	_, err := readRESPResponse(r)
	if err == nil {
		t.Fatal("expected error for oversized array count")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("expected 'too large' error, got %v", err)
	}
}

func TestReadRESPResponse_DepthExceedsMax(t *testing.T) {
	// Build deeply nested arrays: *1\r\n*1\r\n*1\r\n... (11 levels deep).
	var sb strings.Builder
	for i := 0; i <= maxRESPDepth+1; i++ {
		sb.WriteString("*1\r\n")
	}
	sb.WriteString("+OK\r\n") // Leaf value.

	r := bufio.NewReader(strings.NewReader(sb.String()))
	_, err := readRESPResponse(r)
	if err == nil {
		t.Fatal("expected error for excessive nesting depth")
	}
	if !strings.Contains(err.Error(), "depth exceeded") {
		t.Errorf("expected 'depth exceeded' error, got %v", err)
	}
}

func TestReadRESPResponse_MaxDepthAllowed(t *testing.T) {
	// Build arrays at exactly maxRESPDepth — should succeed.
	var sb strings.Builder
	for i := 0; i < maxRESPDepth; i++ {
		sb.WriteString("*1\r\n")
	}
	sb.WriteString("+OK\r\n")

	r := bufio.NewReader(strings.NewReader(sb.String()))
	_, err := readRESPResponse(r)
	if err != nil {
		t.Fatalf("depth %d should be allowed, got error: %v", maxRESPDepth, err)
	}
}

func TestReadRESPResponse_EmptyArray(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("*0\r\n"))
	resp, err := readRESPResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(resp) != "*0\r\n" {
		t.Errorf("expected empty array, got %q", resp)
	}
}

func TestReadRESPResponse_EmptyBulkString(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("$0\r\n\r\n"))
	resp, err := readRESPResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp), "$0\r\n") {
		t.Errorf("expected empty bulk string, got %q", resp)
	}
}

func TestReadRESPCommand_ArrayCountCapped(t *testing.T) {
	// Command with count > 1024 should return empty command (ignored).
	input := "*9999\r\n"
	r := bufio.NewReader(strings.NewReader(input))
	cmd, _, err := readRESPCommand(r)
	if err != nil {
		t.Fatal(err)
	}
	if cmd != "" {
		t.Errorf("expected empty command for oversized count, got %q", cmd)
	}
}

func TestReadRESPCommand_InlineCommand(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("PING\r\n"))
	cmd, _, err := readRESPCommand(r)
	if err != nil {
		t.Fatal(err)
	}
	if cmd != "PING" {
		t.Errorf("expected PING, got %q", cmd)
	}
}

func TestReadRESPCommand_ValidArray(t *testing.T) {
	input := "*2\r\n$3\r\nGET\r\n$3\r\nfoo\r\n"
	r := bufio.NewReader(strings.NewReader(input))
	cmd, raw, err := readRESPCommand(r)
	if err != nil {
		t.Fatal(err)
	}
	if cmd != "GET" {
		t.Errorf("expected GET, got %q", cmd)
	}
	if len(raw) == 0 {
		t.Error("expected raw bytes")
	}
}

func TestReadRESPCommand_ZeroCount(t *testing.T) {
	input := "*0\r\n"
	r := bufio.NewReader(strings.NewReader(input))
	cmd, _, err := readRESPCommand(r)
	if err != nil {
		t.Fatal(err)
	}
	if cmd != "" {
		t.Errorf("expected empty command for zero-count array, got %q", cmd)
	}
}

func TestReadRESPResponse_BulkStringTooLarge(t *testing.T) {
	// Bulk string claiming to be > maxRESPBulkLen should error.
	input := "$999999999999\r\n"
	r := bufio.NewReader(strings.NewReader(input))
	_, err := readRESPResponse(r)
	if err == nil {
		t.Fatal("expected error for oversized bulk string")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("expected 'too large' error, got %v", err)
	}
}

func TestReadRESPResponse_NestedArray(t *testing.T) {
	// *2\r\n*1\r\n+a\r\n+b\r\n — array of [array[a], b]
	input := "*2\r\n*1\r\n+a\r\n+b\r\n"
	r := bufio.NewReader(strings.NewReader(input))
	resp, err := readRESPResponse(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resp), "+a") || !strings.Contains(string(resp), "+b") {
		t.Errorf("expected nested array contents, got %q", resp)
	}
}
