package virtio

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// Vsock implements a virtio-vsock device (virtio spec 5.10).
// Provides socket communication between host and guest.
//
// Queue 0: rx (host→guest)
// Queue 1: tx (guest→host)
// Queue 2: event
type Vsock struct {
	guestCID uint64

	mu    sync.RWMutex
	conns map[vsockConnKey]*vsockConn // Active connections.

	// Pending connections from host Connect() calls.
	pendingMu sync.Mutex
	pending   map[uint32]chan *vsockConn // Port → channel for accept.

	rxQueue *Queue
	notify  func() // Inject interrupt to guest.
}

type vsockConnKey struct {
	srcPort uint32
	dstPort uint32
}

// vsockConn represents one vsock connection.
type vsockConn struct {
	srcCID  uint64
	dstCID  uint64
	srcPort uint32
	dstPort uint32

	// Data buffers.
	readBuf  []byte
	readMu   sync.Mutex
	readCond *sync.Cond

	closed        bool
	readDeadline  time.Time
	writeDeadline time.Time
	mu            sync.Mutex

	vsock *Vsock // Back-reference for sending.
}

// vsock header (virtio_vsock_hdr) is 44 bytes.
const vsockHdrSize = 44

// Vsock operations.
const (
	vsockOpInvalid  = 0
	vsockOpRequest  = 1 // Connection request.
	vsockOpResponse = 2 // Connection response.
	vsockOpRW       = 3 // Data transfer.
	vsockOpShutdown = 4 // Shutdown.
	vsockOpRst      = 5 // Reset / refused.
)

// Vsock types.
const (
	vsockTypeStream = 1
)

// Host CID is always 2.
const hostCID = 2

// NewVsock creates a new virtio-vsock device with the given guest CID.
func NewVsock(guestCID uint64) *Vsock {
	return &Vsock{
		guestCID: guestCID,
		conns:    make(map[vsockConnKey]*vsockConn),
		pending:  make(map[uint32]chan *vsockConn),
	}
}

func (v *Vsock) DeviceID() DeviceID { return DeviceIDVsock }
func (v *Vsock) NumQueues() int     { return 3 }

func (v *Vsock) Features() uint64 {
	return 1 << 32 // VIRTIO_F_VERSION_1
}

func (v *Vsock) ConfigSpace() []byte {
	// guest_cid is a u64 in the config space.
	config := make([]byte, 8)
	binary.LittleEndian.PutUint64(config, v.guestCID)
	return config
}

func (v *Vsock) Reset() {
	v.mu.Lock()
	defer v.mu.Unlock()
	for k, conn := range v.conns {
		conn.mu.Lock()
		conn.closed = true
		conn.mu.Unlock()
		delete(v.conns, k)
	}
}

func (v *Vsock) HandleQueue(queueIdx int, queue *Queue) {
	switch queueIdx {
	case 0:
		// rx queue: host→guest. We track this for sending to guest.
		v.rxQueue = queue
	case 1:
		// tx queue: guest→host.
		v.handleTX(queue)
	case 2:
		// event queue: not used in basic implementation.
	}
}

// SetNotify sets the interrupt injection callback.
func (v *Vsock) SetNotify(notify func()) {
	v.notify = notify
}

// Connect initiates a host→guest connection on the given port.
// Returns a net.Conn for bidirectional communication.
func (v *Vsock) Connect(port uint32) (net.Conn, error) {
	// Allocate an ephemeral source port.
	srcPort := port // Use same port for simplicity.

	conn := &vsockConn{
		srcCID:  hostCID,
		dstCID:  v.guestCID,
		srcPort: srcPort,
		dstPort: port,
		vsock:   v,
	}
	conn.readCond = sync.NewCond(&conn.readMu)

	key := vsockConnKey{srcPort: srcPort, dstPort: port}
	v.mu.Lock()
	v.conns[key] = conn
	v.mu.Unlock()

	// Send connection request to guest.
	v.sendToGuest(vsockOpRequest, conn, nil)

	// Wait for response (with timeout).
	// For now, assume connection succeeds immediately.
	// TODO: proper handshake with response from guest.

	return &vsockNetConn{conn: conn}, nil
}

// handleTX processes packets from the guest.
func (v *Vsock) handleTX(queue *Queue) {
	for {
		head, ok := queue.NextAvail()
		if !ok {
			return
		}

		chain := queue.ReadChain(head)

		// Read the vsock header + data from readable descriptors.
		var packet []byte
		for _, desc := range chain {
			if desc.Flags&VirtqDescFWrite != 0 {
				continue
			}
			data := queue.ReadBuffer(desc.Addr, desc.Len)
			packet = append(packet, data...)
		}

		if len(packet) >= vsockHdrSize {
			v.processGuestPacket(packet)
		}

		queue.PutUsed(head, 0)
	}
}

func (v *Vsock) processGuestPacket(packet []byte) {
	// Parse vsock header.
	srcCID := binary.LittleEndian.Uint64(packet[0:8])
	dstCID := binary.LittleEndian.Uint64(packet[8:16])
	srcPort := binary.LittleEndian.Uint32(packet[16:20])
	dstPort := binary.LittleEndian.Uint32(packet[20:24])
	dataLen := binary.LittleEndian.Uint32(packet[24:28])
	op := binary.LittleEndian.Uint16(packet[30:32])
	_ = srcCID
	_ = dstCID

	key := vsockConnKey{srcPort: dstPort, dstPort: srcPort}

	switch op {
	case vsockOpResponse:
		// Guest accepted our connection.
		v.mu.RLock()
		conn, exists := v.conns[key]
		v.mu.RUnlock()
		if exists {
			conn.readCond.Signal()
		}

	case vsockOpRW:
		// Guest sent data.
		v.mu.RLock()
		conn, exists := v.conns[key]
		v.mu.RUnlock()
		if exists && dataLen > 0 && len(packet) >= int(vsockHdrSize+dataLen) {
			data := packet[vsockHdrSize : vsockHdrSize+dataLen]
			conn.readMu.Lock()
			conn.readBuf = append(conn.readBuf, data...)
			conn.readMu.Unlock()
			conn.readCond.Signal()
		}

	case vsockOpRequest:
		// Guest wants to connect to us — send RST for now.
		// (Host-initiated connections only for supervisor RPC.)
		v.sendRst(srcPort, dstPort)

	case vsockOpShutdown, vsockOpRst:
		v.mu.Lock()
		conn, exists := v.conns[key]
		if exists {
			conn.mu.Lock()
			conn.closed = true
			conn.mu.Unlock()
			conn.readCond.Broadcast()
			delete(v.conns, key)
		}
		v.mu.Unlock()
	}
}

// sendToGuest injects a vsock packet into the guest via the rx queue.
func (v *Vsock) sendToGuest(op uint16, conn *vsockConn, data []byte) {
	if v.rxQueue == nil {
		return
	}

	head, ok := v.rxQueue.NextAvail()
	if !ok {
		return // No guest buffers available.
	}

	chain := v.rxQueue.ReadChain(head)

	// Build vsock header.
	hdr := make([]byte, vsockHdrSize)
	binary.LittleEndian.PutUint64(hdr[0:8], conn.srcCID)   // src_cid
	binary.LittleEndian.PutUint64(hdr[8:16], conn.dstCID)  // dst_cid
	binary.LittleEndian.PutUint32(hdr[16:20], conn.srcPort) // src_port
	binary.LittleEndian.PutUint32(hdr[20:24], conn.dstPort) // dst_port
	binary.LittleEndian.PutUint32(hdr[24:28], uint32(len(data))) // len
	binary.LittleEndian.PutUint16(hdr[28:30], vsockTypeStream)   // type
	binary.LittleEndian.PutUint16(hdr[30:32], op)                // op
	// Remaining fields (flags, buf_alloc, fwd_cnt) = 0.

	packet := append(hdr, data...)

	// Write into guest-writable descriptors.
	var written uint32
	offset := 0
	for _, desc := range chain {
		if desc.Flags&VirtqDescFWrite == 0 {
			continue
		}
		toWrite := len(packet) - offset
		if toWrite <= 0 {
			break
		}
		if toWrite > int(desc.Len) {
			toWrite = int(desc.Len)
		}
		v.rxQueue.WriteBuffer(desc.Addr, packet[offset:offset+toWrite])
		offset += toWrite
		written += uint32(toWrite)
	}

	v.rxQueue.PutUsed(head, written)
	if v.notify != nil {
		v.notify()
	}
}

func (v *Vsock) sendRst(srcPort, dstPort uint32) {
	conn := &vsockConn{
		srcCID:  hostCID,
		dstCID:  v.guestCID,
		srcPort: dstPort,
		dstPort: srcPort,
	}
	v.sendToGuest(vsockOpRst, conn, nil)
}

// vsockNetConn wraps vsockConn as a net.Conn.
type vsockNetConn struct {
	conn *vsockConn
}

func (c *vsockNetConn) Read(b []byte) (int, error) {
	c.conn.readMu.Lock()
	defer c.conn.readMu.Unlock()

	for len(c.conn.readBuf) == 0 {
		c.conn.mu.Lock()
		closed := c.conn.closed
		deadline := c.conn.readDeadline
		c.conn.mu.Unlock()
		if closed {
			return 0, io.EOF
		}

		// Check if deadline has passed.
		if !deadline.IsZero() && time.Now().After(deadline) {
			return 0, os.ErrDeadlineExceeded
		}

		if !deadline.IsZero() {
			// Timed wait: use a goroutine + timer to wake readCond.
			timeout := time.Until(deadline)
			if timeout <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			timer := time.AfterFunc(timeout, func() {
				c.conn.readCond.Broadcast()
			})
			c.conn.readCond.Wait()
			timer.Stop()
		} else {
			c.conn.readCond.Wait()
		}
	}

	n := copy(b, c.conn.readBuf)
	c.conn.readBuf = c.conn.readBuf[n:]
	return n, nil
}

func (c *vsockNetConn) Write(b []byte) (int, error) {
	c.conn.mu.Lock()
	closed := c.conn.closed
	c.conn.mu.Unlock()
	if closed {
		return 0, fmt.Errorf("connection closed")
	}

	c.conn.vsock.sendToGuest(vsockOpRW, c.conn, b)
	return len(b), nil
}

func (c *vsockNetConn) Close() error {
	c.conn.vsock.sendToGuest(vsockOpShutdown, c.conn, nil)
	c.conn.mu.Lock()
	c.conn.closed = true
	c.conn.mu.Unlock()
	c.conn.readCond.Broadcast()

	key := vsockConnKey{srcPort: c.conn.srcPort, dstPort: c.conn.dstPort}
	c.conn.vsock.mu.Lock()
	delete(c.conn.vsock.conns, key)
	c.conn.vsock.mu.Unlock()

	return nil
}

func (c *vsockNetConn) LocalAddr() net.Addr {
	return &vsockAddr{cid: c.conn.srcCID, port: c.conn.srcPort}
}

func (c *vsockNetConn) RemoteAddr() net.Addr {
	return &vsockAddr{cid: c.conn.dstCID, port: c.conn.dstPort}
}

func (c *vsockNetConn) SetDeadline(t time.Time) error {
	c.conn.mu.Lock()
	c.conn.readDeadline = t
	c.conn.writeDeadline = t
	c.conn.mu.Unlock()
	// Wake any blocked readers so they recheck the deadline.
	c.conn.readCond.Broadcast()
	return nil
}

func (c *vsockNetConn) SetReadDeadline(t time.Time) error {
	c.conn.mu.Lock()
	c.conn.readDeadline = t
	c.conn.mu.Unlock()
	c.conn.readCond.Broadcast()
	return nil
}

func (c *vsockNetConn) SetWriteDeadline(t time.Time) error {
	c.conn.mu.Lock()
	c.conn.writeDeadline = t
	c.conn.mu.Unlock()
	return nil
}

type vsockAddr struct {
	cid  uint64
	port uint32
}

func (a *vsockAddr) Network() string { return "vsock" }
func (a *vsockAddr) String() string  { return fmt.Sprintf("%d:%d", a.cid, a.port) }
