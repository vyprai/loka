package dbproxy

import (
	"encoding/binary"
	"io"
	"net"
	"strings"
	"time"
)

// MySQL command bytes.
const (
	comQuery       byte = 0x03
	comInitDB      byte = 0x02
	comStmtPrepare byte = 0x16
	comStmtExecute byte = 0x17
	comQuit        byte = 0x01
)

// MySQLProxy handles a single MySQL client connection with read/write split.
type MySQLProxy struct {
	route     *Route
	txnPinned bool
	stmtMap   map[uint32]bool // stmt_id → isRead
	primary   net.Conn
	replica   net.Conn
}

func newMySQLProxy(route *Route) *MySQLProxy {
	return &MySQLProxy{
		route:   route,
		stmtMap: make(map[uint32]bool),
	}
}

// HandleConnection manages the full MySQL client lifecycle.
func (mp *MySQLProxy) HandleConnection(clientConn net.Conn) {
	defer clientConn.Close()

	// Connect to primary for handshake.
	primaryBackend := mp.route.PickBackend(false)
	if primaryBackend == nil {
		return
	}
	primaryConn, err := net.DialTimeout("tcp", primaryBackend.Address, 5*time.Second)
	if err != nil {
		return
	}
	defer primaryConn.Close()
	mp.primary = primaryConn

	// Pass through MySQL handshake (server greeting → client auth → OK/ERR).
	if err := mp.passHandshake(clientConn, primaryConn); err != nil {
		return
	}

	// Query loop.
	for {
		pkt, err := readMySQLPacket(clientConn)
		if err != nil {
			return
		}
		if len(pkt) == 0 {
			continue
		}

		cmdByte := pkt[0]
		if cmdByte == comQuit {
			return
		}

		isRead := mp.classifyPacket(cmdByte, pkt[1:])
		backend := mp.pickBackendConn(isRead)

		// Forward packet to backend (re-wrap with sequence 0).
		if err := writeMySQLPacket(backend, 0, pkt); err != nil {
			return
		}

		// Relay response to client.
		if err := mp.relayResponse(backend, clientConn); err != nil {
			return
		}
	}
}

// passHandshake forwards the MySQL auth handshake.
func (mp *MySQLProxy) passHandshake(client, primary net.Conn) error {
	// Server greeting (from primary → client).
	greeting, err := readMySQLRawPacket(primary)
	if err != nil {
		return err
	}
	if _, err := client.Write(greeting); err != nil {
		return err
	}

	// Client auth response (from client → primary).
	authResp, err := readMySQLRawPacket(client)
	if err != nil {
		return err
	}
	if _, err := primary.Write(authResp); err != nil {
		return err
	}

	// Server OK/ERR (may be multiple packets for auth switch).
	for {
		resp, err := readMySQLRawPacket(primary)
		if err != nil {
			return err
		}
		if _, err := client.Write(resp); err != nil {
			return err
		}
		// Check if this is OK (0x00) or ERR (0xFF) packet.
		if len(resp) > 4 {
			switch resp[4] {
			case 0x00, 0xFF: // OK or ERR — handshake complete.
				return nil
			case 0xFE: // Auth switch request — continue.
				// Read client's auth switch response.
				switchResp, err := readMySQLRawPacket(client)
				if err != nil {
					return err
				}
				if _, err := primary.Write(switchResp); err != nil {
					return err
				}
			}
		}
	}
}

// classifyPacket determines if a MySQL command packet is a read.
func (mp *MySQLProxy) classifyPacket(cmd byte, payload []byte) bool {
	switch cmd {
	case comQuery:
		sql := string(payload)
		return mp.classifySQL(sql)

	case comStmtPrepare:
		sql := string(payload)
		isRead := mp.classifySQL(sql)
		// stmt_id is in the response; we'll track it when we see the OK_STMT response.
		// For now, treat prepare as the query type.
		return isRead

	case comStmtExecute:
		if len(payload) >= 4 {
			stmtID := binary.LittleEndian.Uint32(payload[:4])
			if isRead, ok := mp.stmtMap[stmtID]; ok {
				return isRead
			}
		}
		return false

	case comInitDB:
		return false // Switch database — forward to all backends.

	default:
		return false
	}
}

// classifySQL checks if a SQL statement is a read.
func (mp *MySQLProxy) classifySQL(sql string) bool {
	upper := strings.ToUpper(strings.TrimSpace(sql))

	// COMMIT/ROLLBACK must be checked BEFORE the txnPinned early-return
	// so that they can reset the pin state.
	if strings.HasPrefix(upper, "COMMIT") || strings.HasPrefix(upper, "ROLLBACK") {
		mp.txnPinned = false
		return false
	}

	if mp.txnPinned {
		return false
	}

	if strings.HasPrefix(upper, "BEGIN") ||
		strings.HasPrefix(upper, "START TRANSACTION") ||
		strings.HasPrefix(upper, "SET") {
		mp.txnPinned = true
		return false
	}

	if strings.HasPrefix(upper, "SELECT") {
		if strings.Contains(upper, "FOR UPDATE") || strings.Contains(upper, "FOR SHARE") {
			return false
		}
		return true
	}
	if strings.HasPrefix(upper, "SHOW") || strings.HasPrefix(upper, "EXPLAIN") {
		return true
	}

	return false
}

func (mp *MySQLProxy) pickBackendConn(isRead bool) net.Conn {
	if !isRead || mp.txnPinned {
		return mp.primary
	}
	if mp.replica == nil {
		replicaBackend := mp.route.PickBackend(true)
		if replicaBackend == nil || replicaBackend == mp.route.Primary {
			return mp.primary
		}
		conn, err := net.DialTimeout("tcp", replicaBackend.Address, 5*time.Second)
		if err != nil {
			return mp.primary
		}
		mp.replica = conn
	}
	return mp.replica
}

// relayResponse copies MySQL response packets from backend to client.
func (mp *MySQLProxy) relayResponse(backend, client net.Conn) error {
	// Read at least one response packet.
	resp, err := readMySQLRawPacket(backend)
	if err != nil {
		return err
	}
	if _, err := client.Write(resp); err != nil {
		return err
	}

	// Check if it's OK (single packet), ERR (single packet), or result set (multi-packet).
	if len(resp) > 4 {
		switch resp[4] {
		case 0x00: // OK packet — done.
			return nil
		case 0xFF: // ERR packet — done.
			return nil
		}
	}

	// Result set: relay until EOF/OK marker.
	// This is simplified — full implementation would track field count + row packets.
	// For now, relay packets until we see an EOF (0xFE) or OK (0x00) with appropriate flags.
	for {
		pkt, err := readMySQLRawPacket(backend)
		if err != nil {
			return err
		}
		if _, err := client.Write(pkt); err != nil {
			return err
		}
		// Check for EOF/OK terminator.
		if len(pkt) > 4 && (pkt[4] == 0xFE || pkt[4] == 0x00) && len(pkt) < 16 {
			return nil
		}
	}
}

// readMySQLPacket reads a MySQL packet and returns just the payload.
func readMySQLPacket(conn net.Conn) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	payloadLen := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	if payloadLen > 16*1024*1024 { // 16MB MySQL packet limit
		return nil, io.ErrUnexpectedEOF
	}
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// readMySQLRawPacket reads a full MySQL packet including the 4-byte header.
func readMySQLRawPacket(conn net.Conn) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	payloadLen := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	if payloadLen > 16*1024*1024 {
		return nil, io.ErrUnexpectedEOF
	}
	pkt := make([]byte, 4+payloadLen)
	copy(pkt, header[:])
	if payloadLen > 0 {
		if _, err := io.ReadFull(conn, pkt[4:]); err != nil {
			return nil, err
		}
	}
	return pkt, nil
}

// writeMySQLPacket writes a MySQL packet with the given sequence number.
func writeMySQLPacket(conn net.Conn, seqID byte, payload []byte) error {
	header := make([]byte, 4)
	header[0] = byte(len(payload))
	header[1] = byte(len(payload) >> 8)
	header[2] = byte(len(payload) >> 16)
	header[3] = seqID
	if _, err := conn.Write(header); err != nil {
		return err
	}
	if _, err := conn.Write(payload); err != nil {
		return err
	}
	return nil
}
