package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	pb "github.com/vyprai/loka/api/lokav1"
	"github.com/spf13/cobra"
)

func newSessionPortForwardCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "port-forward <session-id> <local:remote> [local:remote...]",
		Short: "Forward local ports to a session VM",
		Long: `Tunnel TCP connections from your machine to ports inside a running session.

Examples:
  loka session port-forward <id> 8080:5000              # localhost:8080 → VM:5000
  loka session port-forward <id> 8080:5000 3001:3001    # Multiple ports
  loka session port-forward <id> 0:5000                 # Auto-assign local port`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionID := args[0]
			var mappings []portMap
			for _, arg := range args[1:] {
				pm, err := parsePortMap(arg)
				if err != nil {
					return err
				}
				mappings = append(mappings, pm)
			}

			grpcClient := newGRPCClient()
			if grpcClient == nil {
				return fmt.Errorf("cannot connect via gRPC — port-forward requires gRPC")
			}
			defer grpcClient.Close()

			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			var wg sync.WaitGroup
			for _, pm := range mappings {
				wg.Add(1)
				go func(pm portMap) {
					defer wg.Done()
					if err := runPortForward(ctx, grpcClient.Proto(), sessionID, pm); err != nil {
						fmt.Printf("port-forward %d:%d error: %v\n", pm.local, pm.remote, err)
					}
				}(pm)
			}

			wg.Wait()
			return nil
		},
	}
}

type portMap struct {
	local  int
	remote int
}

func parsePortMap(s string) (portMap, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return portMap{}, fmt.Errorf("invalid port mapping %q (expected local:remote)", s)
	}
	local, err := strconv.Atoi(parts[0])
	if err != nil {
		return portMap{}, fmt.Errorf("invalid local port %q", parts[0])
	}
	remote, err := strconv.Atoi(parts[1])
	if err != nil {
		return portMap{}, fmt.Errorf("invalid remote port %q", parts[1])
	}
	return portMap{local: local, remote: remote}, nil
}

func runPortForward(ctx context.Context, client pb.ControlServiceClient, sessionID string, pm portMap) error {
	// Open gRPC stream.
	stream, err := client.PortForward(ctx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	// Send init.
	if err := stream.Send(&pb.PortForwardMessage{
		SessionId: sessionID,
		Payload: &pb.PortForwardMessage_Init{
			Init: &pb.PortForwardInit{
				LocalPort:  int32(pm.local),
				RemotePort: int32(pm.remote),
			},
		},
	}); err != nil {
		return fmt.Errorf("send init: %w", err)
	}

	// Start local TCP listener.
	listenAddr := fmt.Sprintf("127.0.0.1:%d", pm.local)
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listenAddr, err)
	}
	defer listener.Close()

	actualAddr := listener.Addr().(*net.TCPAddr)
	fmt.Printf("Forwarding %s → session %s port %d\n", actualAddr, shortID(sessionID), pm.remote)

	// Connection tracking.
	var nextConnID atomic.Uint32
	var mu sync.Mutex
	conns := make(map[uint32]net.Conn)

	// Goroutine: receive from gRPC stream → write to local TCP connections.
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			switch p := msg.Payload.(type) {
			case *pb.PortForwardMessage_Data:
				mu.Lock()
				conn, ok := conns[p.Data.ConnectionId]
				mu.Unlock()
				if ok {
					conn.Write(p.Data.Data)
				}
			case *pb.PortForwardMessage_Close:
				mu.Lock()
				conn, ok := conns[p.Close.ConnectionId]
				delete(conns, p.Close.ConnectionId)
				mu.Unlock()
				if ok {
					conn.Close()
				}
			case *pb.PortForwardMessage_Error:
				fmt.Printf("  port-forward error (conn %d): %s\n", p.Error.ConnectionId, p.Error.Message)
				mu.Lock()
				conn, ok := conns[p.Error.ConnectionId]
				delete(conns, p.Error.ConnectionId)
				mu.Unlock()
				if ok {
					conn.Close()
				}
			}
		}
	}()

	// Accept loop: accept local TCP connections, relay data over gRPC.
	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("accept: %w", err)
			}
		}

		connID := nextConnID.Add(1)
		mu.Lock()
		conns[connID] = conn
		mu.Unlock()

		// Goroutine: read from local TCP → send over gRPC.
		go func(connID uint32, conn net.Conn) {
			defer func() {
				conn.Close()
				mu.Lock()
				delete(conns, connID)
				mu.Unlock()
				stream.Send(&pb.PortForwardMessage{
					SessionId: sessionID,
					Payload: &pb.PortForwardMessage_Close{
						Close: &pb.PortForwardClose{ConnectionId: connID},
					},
				})
			}()

			buf := make([]byte, 32*1024)
			for {
				n, err := conn.Read(buf)
				if n > 0 {
					stream.Send(&pb.PortForwardMessage{
						SessionId: sessionID,
						Payload: &pb.PortForwardMessage_Data{
							Data: &pb.PortForwardData{
								ConnectionId: connID,
								Data:         buf[:n],
							},
						},
					})
				}
				if err == io.EOF {
					return
				}
				if err != nil {
					return
				}
			}
		}(connID, conn)
	}
}
