package vm

import (
	"fmt"
	"io"
	"net"
	"sync"
)

// PortForwarder provides userspace TCP/UDP port forwarding.
type PortForwarder struct {
	listeners map[int]io.Closer
	mu        sync.Mutex
}

// NewPortForwarder creates a new PortForwarder.
func NewPortForwarder() *PortForwarder {
	return &PortForwarder{
		listeners: make(map[int]io.Closer),
	}
}

// Forward starts forwarding from hostPort on localhost to guestIP:guestPort.
// Supported protocols: "tcp" and "udp".
func (pf *PortForwarder) Forward(hostPort int, guestIP string, guestPort int, proto string) error {
	pf.mu.Lock()
	if _, exists := pf.listeners[hostPort]; exists {
		pf.mu.Unlock()
		return fmt.Errorf("port %d is already being forwarded", hostPort)
	}
	pf.mu.Unlock()

	switch proto {
	case "tcp", "":
		return pf.forwardTCP(hostPort, guestIP, guestPort)
	case "udp":
		return pf.forwardUDP(hostPort, guestIP, guestPort)
	default:
		return fmt.Errorf("unsupported protocol %q", proto)
	}
}

func (pf *PortForwarder) forwardTCP(hostPort int, guestIP string, guestPort int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", hostPort))
	if err != nil {
		return fmt.Errorf("listen tcp %d: %w", hostPort, err)
	}

	pf.mu.Lock()
	pf.listeners[hostPort] = listener
	pf.mu.Unlock()

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return // listener closed
			}
			go pf.handleTCPConn(conn, guestIP, guestPort)
		}
	}()

	return nil
}

func (pf *PortForwarder) handleTCPConn(local net.Conn, guestIP string, guestPort int) {
	defer local.Close()

	remote, err := net.Dial("tcp", fmt.Sprintf("%s:%d", guestIP, guestPort))
	if err != nil {
		return
	}
	defer remote.Close()

	var wg sync.WaitGroup
	wg.Add(2)

	// local -> remote
	go func() {
		defer wg.Done()
		io.Copy(remote, local)
	}()

	// remote -> local
	go func() {
		defer wg.Done()
		io.Copy(local, remote)
	}()

	wg.Wait()
}

func (pf *PortForwarder) forwardUDP(hostPort int, guestIP string, guestPort int) error {
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", hostPort))
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("listen udp %d: %w", hostPort, err)
	}

	pf.mu.Lock()
	pf.listeners[hostPort] = conn
	pf.mu.Unlock()

	remoteAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", guestIP, guestPort))
	if err != nil {
		conn.Close()
		return err
	}

	go func() {
		buf := make([]byte, 65535)
		for {
			n, clientAddr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return // conn closed
			}

			// Forward to guest. For simplicity, use a per-packet dial.
			// A production implementation would maintain client->remote mappings.
			remote, err := net.DialUDP("udp", nil, remoteAddr)
			if err != nil {
				continue
			}
			remote.Write(buf[:n])

			// Read reply and send back to client.
			go func(clientAddr *net.UDPAddr) {
				defer remote.Close()
				reply := make([]byte, 65535)
				n, err := remote.Read(reply)
				if err != nil {
					return
				}
				conn.WriteToUDP(reply[:n], clientAddr)
			}(clientAddr)
		}
	}()

	return nil
}

// Stop stops forwarding on the given host port.
func (pf *PortForwarder) Stop(hostPort int) error {
	pf.mu.Lock()
	defer pf.mu.Unlock()

	closer, ok := pf.listeners[hostPort]
	if !ok {
		return fmt.Errorf("no forwarder on port %d", hostPort)
	}
	delete(pf.listeners, hostPort)
	return closer.Close()
}

// StopAll stops all active port forwards.
func (pf *PortForwarder) StopAll() error {
	pf.mu.Lock()
	defer pf.mu.Unlock()

	var firstErr error
	for port, closer := range pf.listeners {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(pf.listeners, port)
	}
	return firstErr
}
