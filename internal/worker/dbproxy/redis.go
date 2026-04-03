package dbproxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// readCommands is the set of Redis commands that are safe to route to replicas.
var readCommands = map[string]bool{
	"GET": true, "MGET": true, "STRLEN": true, "GETRANGE": true,
	"HGET": true, "HMGET": true, "HGETALL": true, "HKEYS": true,
	"HVALS": true, "HLEN": true, "HEXISTS": true, "HSCAN": true,
	"LRANGE": true, "LLEN": true, "LINDEX": true, "LPOS": true,
	"SCARD": true, "SISMEMBER": true, "SMISMEMBER": true, "SMEMBERS": true,
	"SRANDMEMBER": true, "SSCAN": true,
	"ZCARD": true, "ZCOUNT": true, "ZRANGE": true, "ZRANGEBYSCORE": true,
	"ZRANGEBYLEX": true, "ZRANK": true, "ZREVRANGE": true, "ZREVRANGEBYSCORE": true,
	"ZREVRANK": true, "ZSCORE": true, "ZMSCORE": true, "ZSCAN": true,
	"EXISTS": true, "TYPE": true, "TTL": true, "PTTL": true,
	"KEYS": true, "SCAN": true, "DBSIZE": true, "RANDOMKEY": true,
	"OBJECT": true, "DEBUG": true, "MEMORY": true,
	"XRANGE": true, "XREVRANGE": true, "XLEN": true, "XINFO": true,
	"XREAD": true, "XPENDING": true,
	"PFCOUNT": true,
	"GEORADIUS_RO": true, "GEOSEARCH": true, "GEODIST": true,
	"GEOPOS": true, "GEOHASH": true,
	"BITCOUNT": true, "BITPOS": true, "GETBIT": true,
	"PING": true, "ECHO": true, "TIME": true, "INFO": true,
}

// RedisProxy handles a single Redis client connection with read/write split.
type RedisProxy struct {
	route     *Route
	txnPinned bool // True inside MULTI..EXEC.
	primary   net.Conn
	replica   net.Conn
}

func newRedisProxy(route *Route) *RedisProxy {
	return &RedisProxy{route: route}
}

// HandleConnection manages the Redis client lifecycle.
func (rp *RedisProxy) HandleConnection(clientConn net.Conn) {
	defer clientConn.Close()

	// Connect to primary.
	primaryBackend := rp.route.PickBackend(false)
	if primaryBackend == nil {
		return
	}
	primaryConn, err := net.DialTimeout("tcp", primaryBackend.Address, 5*time.Second)
	if err != nil {
		return
	}
	defer primaryConn.Close()
	rp.primary = primaryConn

	reader := bufio.NewReader(clientConn)

	for {
		// Read one RESP command from client.
		cmd, raw, err := readRESPCommand(reader)
		if err != nil {
			return
		}

		isRead := rp.classifyCommand(cmd)
		backend := rp.pickBackendConn(isRead)

		// Forward raw bytes to backend.
		if _, err := backend.Write(raw); err != nil {
			return
		}

		// Read response from backend, forward to client.
		resp, err := readRESPResponse(bufio.NewReader(backend))
		if err != nil {
			return
		}
		if _, err := clientConn.Write(resp); err != nil {
			return
		}
	}
}

// classifyCommand determines if a Redis command is a read.
func (rp *RedisProxy) classifyCommand(cmd string) bool {
	upper := strings.ToUpper(cmd)

	if rp.txnPinned {
		if upper == "EXEC" || upper == "DISCARD" {
			rp.txnPinned = false
		}
		return false // Everything inside MULTI goes to primary.
	}

	if upper == "MULTI" {
		rp.txnPinned = true
		return false
	}

	// SUBSCRIBE/PSUBSCRIBE pin to one backend.
	if upper == "SUBSCRIBE" || upper == "PSUBSCRIBE" || upper == "UNSUBSCRIBE" || upper == "PUNSUBSCRIBE" {
		return false
	}

	return readCommands[upper]
}

func (rp *RedisProxy) pickBackendConn(isRead bool) net.Conn {
	if !isRead || rp.txnPinned {
		return rp.primary
	}
	if rp.replica == nil {
		replicaBackend := rp.route.PickBackend(true)
		if replicaBackend == nil || replicaBackend == rp.route.Primary {
			return rp.primary
		}
		conn, err := net.DialTimeout("tcp", replicaBackend.Address, 5*time.Second)
		if err != nil {
			return rp.primary
		}
		rp.replica = conn
	}
	return rp.replica
}

// readRESPCommand reads a RESP command array and returns the command name + raw bytes.
func readRESPCommand(r *bufio.Reader) (string, []byte, error) {
	// RESP array: *N\r\n$len\r\narg\r\n...
	var raw []byte

	line, err := r.ReadBytes('\n')
	if err != nil {
		return "", nil, err
	}
	raw = append(raw, line...)

	if len(line) < 2 || line[0] != '*' {
		// Inline command: just the command text.
		cmd := strings.Fields(strings.TrimSpace(string(line)))
		if len(cmd) == 0 {
			return "", raw, nil
		}
		return cmd[0], raw, nil
	}

	// Array: parse count, read elements.
	count := 0
	for _, b := range line[1 : len(line)-2] { // skip '*' and '\r\n'
		count = count*10 + int(b-'0')
	}
	if count <= 0 || count > 1024 {
		return "", raw, nil
	}

	var cmdName string
	for i := 0; i < count; i++ {
		// Read $len\r\n
		lenLine, err := r.ReadBytes('\n')
		if err != nil {
			return "", nil, err
		}
		raw = append(raw, lenLine...)

		argLen := 0
		for _, b := range lenLine[1 : len(lenLine)-2] {
			argLen = argLen*10 + int(b-'0')
		}

		// Read arg + \r\n
		arg := make([]byte, argLen+2)
		if _, err := io.ReadFull(r, arg); err != nil {
			return "", nil, err
		}
		raw = append(raw, arg...)

		if i == 0 {
			cmdName = string(arg[:argLen])
		}
	}

	return cmdName, raw, nil
}

const (
	maxRESPArrayCount = 65536  // Max elements in a RESP array.
	maxRESPBulkLen    = 512 << 20 // 512 MB max bulk string.
	maxRESPDepth      = 10     // Max nesting depth for arrays.
)

// readRESPResponse reads a full RESP response (may be multi-line for arrays).
func readRESPResponse(r *bufio.Reader) ([]byte, error) {
	return readRESPResponseDepth(r, 0)
}

func readRESPResponseDepth(r *bufio.Reader, depth int) ([]byte, error) {
	if depth > maxRESPDepth {
		return nil, fmt.Errorf("RESP nesting depth exceeded (%d)", maxRESPDepth)
	}

	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}

	if len(line) == 0 {
		return line, nil
	}

	switch line[0] {
	case '+', '-', ':': // Simple string, error, integer — single line.
		return line, nil

	case '$': // Bulk string.
		result := make([]byte, len(line))
		copy(result, line)
		bulkLen := 0
		neg := false
		for _, b := range line[1 : len(line)-2] {
			if b == '-' {
				neg = true
				continue
			}
			bulkLen = bulkLen*10 + int(b-'0')
		}
		if neg {
			return result, nil // $-1 = null bulk string.
		}
		if bulkLen > maxRESPBulkLen {
			return nil, fmt.Errorf("RESP bulk string too large: %d", bulkLen)
		}
		data := make([]byte, bulkLen+2) // +2 for trailing \r\n
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, err
		}
		return append(result, data...), nil

	case '*': // Array — read count, then read that many elements.
		result := make([]byte, len(line))
		copy(result, line)
		count := 0
		neg := false
		for _, b := range line[1 : len(line)-2] {
			if b == '-' {
				neg = true
				continue
			}
			count = count*10 + int(b-'0')
		}
		if neg {
			return result, nil // *-1 = null array.
		}
		if count > maxRESPArrayCount {
			return nil, fmt.Errorf("RESP array too large: %d (max %d)", count, maxRESPArrayCount)
		}
		for i := 0; i < count; i++ {
			elem, err := readRESPResponseDepth(r, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, elem...)
		}
		return result, nil

	default:
		return line, nil
	}
}
