package dbproxy

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"time"
)

// PostgresProxy handles a single postgres client connection, inspecting
// queries to route reads to replicas and writes to primary.
type PostgresProxy struct {
	route     *Route
	txnPinned bool              // True inside BEGIN..COMMIT — all queries go to primary.
	prepStmts map[string]bool   // prepared statement name → isRead
	primary   net.Conn          // persistent connection to primary
	replica   net.Conn          // persistent connection to current read replica
}

func newPostgresProxy(route *Route) *PostgresProxy {
	return &PostgresProxy{
		route:     route,
		prepStmts: make(map[string]bool),
	}
}

// HandleConnection manages the full lifecycle of a postgres client connection.
func (pp *PostgresProxy) HandleConnection(clientConn net.Conn) {
	defer clientConn.Close()

	// Phase 1: Startup — always connect to primary for auth handshake.
	primaryBackend := pp.route.PickBackend(false)
	if primaryBackend == nil {
		return
	}
	primaryConn, err := net.DialTimeout("tcp", primaryBackend.Address, 5*time.Second)
	if err != nil {
		return
	}
	defer primaryConn.Close()
	pp.primary = primaryConn

	// Pass through the startup sequence (SSL negotiation + auth).
	// Client sends StartupMessage, we forward to primary, relay responses back.
	if err := pp.passStartup(clientConn, primaryConn); err != nil {
		return
	}

	// Phase 2: Query loop — inspect each message from client.
	for {
		msgType, payload, err := readPgMessage(clientConn)
		if err != nil {
			return
		}

		isRead := pp.classifyMessage(msgType, payload)
		backend := pp.pickBackendConn(isRead)

		// Forward message to chosen backend.
		if err := writePgMessage(backend, msgType, payload); err != nil {
			return
		}

		// Relay response back to client until ReadyForQuery.
		if err := pp.relayResponse(backend, clientConn); err != nil {
			return
		}
	}
}

// passStartup forwards the startup handshake between client and primary.
func (pp *PostgresProxy) passStartup(client, primary net.Conn) error {
	// Read startup message from client (no type byte, just length + payload).
	var lenBuf [4]byte
	if _, err := io.ReadFull(client, lenBuf[:]); err != nil {
		return err
	}
	msgLen := int(binary.BigEndian.Uint32(lenBuf[:])) - 4
	if msgLen < 0 || msgLen > 10000 {
		return io.ErrUnexpectedEOF
	}
	payload := make([]byte, msgLen)
	if _, err := io.ReadFull(client, payload); err != nil {
		return err
	}

	// Forward to primary.
	primary.Write(lenBuf[:])
	primary.Write(payload)

	// Relay auth responses back to client until ReadyForQuery ('Z').
	for {
		msgType, respPayload, err := readPgMessage(primary)
		if err != nil {
			return err
		}
		if err := writePgMessage(client, msgType, respPayload); err != nil {
			return err
		}
		if msgType == 'Z' { // ReadyForQuery
			return nil
		}
	}
}

// classifyMessage determines if a frontend message represents a read query.
func (pp *PostgresProxy) classifyMessage(msgType byte, payload []byte) bool {
	switch msgType {
	case 'Q': // Simple Query
		sql := string(payload[:len(payload)-1]) // strip null terminator
		return pp.classifySQL(sql)

	case 'P': // Parse (extended query: prepared statement)
		// Format: stmt_name\0 query\0 param_count...
		parts := strings.SplitN(string(payload), "\x00", 3)
		if len(parts) >= 2 {
			stmtName := parts[0]
			sql := parts[1]
			isRead := pp.classifySQL(sql)
			// Evict oldest if map grows too large (prevents memory leak).
			if len(pp.prepStmts) >= 1000 {
				for k := range pp.prepStmts {
					delete(pp.prepStmts, k)
					break
				}
			}
			pp.prepStmts[stmtName] = isRead
			return isRead
		}
		return false

	case 'B': // Bind — uses previously parsed statement
		// portal\0 stmt\0 ...
		parts := strings.SplitN(string(payload), "\x00", 3)
		if len(parts) >= 2 {
			stmtName := parts[1]
			if isRead, ok := pp.prepStmts[stmtName]; ok {
				return isRead
			}
		}
		return false

	case 'E': // Execute — follows Bind, no SQL to classify
		return false // Conservatively route to primary.

	default:
		return false // Unknown messages go to primary.
	}
}

// classifySQL checks if a SQL statement is a read query.
func (pp *PostgresProxy) classifySQL(sql string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sql))

	// COMMIT/ROLLBACK must be checked before the txnPinned early return,
	// otherwise they can never unpin the transaction.
	if strings.HasPrefix(upper, "COMMIT") || strings.HasPrefix(upper, "END") ||
		strings.HasPrefix(upper, "ROLLBACK") || strings.HasPrefix(upper, "ABORT") {
		pp.txnPinned = false
		return false
	}

	if pp.txnPinned {
		return false // Inside transaction — everything to primary.
	}

	// Transaction control — pin to primary.
	if strings.HasPrefix(upper, "BEGIN") ||
		strings.HasPrefix(upper, "START TRANSACTION") ||
		strings.HasPrefix(upper, "SET") {
		pp.txnPinned = true
		return false
	}

	// Read queries.
	if strings.HasPrefix(upper, "SELECT") ||
		strings.HasPrefix(upper, "SHOW") ||
		strings.HasPrefix(upper, "EXPLAIN") ||
		strings.HasPrefix(upper, "FETCH") {
		// SELECT ... FOR UPDATE/SHARE is a write.
		if strings.Contains(upper, "FOR UPDATE") || strings.Contains(upper, "FOR SHARE") {
			return false
		}
		return true
	}

	if strings.HasPrefix(upper, "COPY") && strings.Contains(upper, "TO") {
		return true
	}

	return false // Everything else is a write.
}

// pickBackendConn returns the appropriate backend connection.
func (pp *PostgresProxy) pickBackendConn(isRead bool) net.Conn {
	if !isRead || pp.txnPinned {
		return pp.primary
	}

	// Lazy-connect to a replica.
	if pp.replica == nil {
		replicaBackend := pp.route.PickBackend(true)
		if replicaBackend == nil || replicaBackend == pp.route.Primary {
			return pp.primary
		}
		conn, err := net.DialTimeout("tcp", replicaBackend.Address, 5*time.Second)
		if err != nil {
			return pp.primary // Fallback to primary.
		}
		pp.replica = conn
	}
	return pp.replica
}

// relayResponse copies backend responses to client until ReadyForQuery.
func (pp *PostgresProxy) relayResponse(backend, client net.Conn) error {
	for {
		msgType, payload, err := readPgMessage(backend)
		if err != nil {
			return err
		}
		if err := writePgMessage(client, msgType, payload); err != nil {
			return err
		}
		if msgType == 'Z' { // ReadyForQuery
			if len(payload) > 0 {
				switch payload[0] {
				case 'I': // Idle — not in transaction.
					pp.txnPinned = false
				case 'T': // In transaction.
					pp.txnPinned = true
				case 'E': // Failed transaction.
					pp.txnPinned = true
				}
			}
			return nil
		}
	}
}

// readPgMessage reads a single postgres protocol message (type byte + length + payload).
func readPgMessage(conn net.Conn) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return 0, nil, err
	}
	msgType := header[0]
	msgLen := int(binary.BigEndian.Uint32(header[1:])) - 4
	if msgLen < 0 {
		msgLen = 0
	}
	if msgLen > 100*1024*1024 { // 100MB sanity limit
		return 0, nil, io.ErrUnexpectedEOF
	}
	payload := make([]byte, msgLen)
	if msgLen > 0 {
		if _, err := io.ReadFull(conn, payload); err != nil {
			return 0, nil, err
		}
	}
	return msgType, payload, nil
}

// writePgMessage writes a postgres protocol message.
func writePgMessage(conn net.Conn, msgType byte, payload []byte) error {
	var header [5]byte
	header[0] = msgType
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)+4))
	if _, err := conn.Write(header[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := conn.Write(payload); err != nil {
			return err
		}
	}
	return nil
}
