package main

import (
	"log"

	"github.com/vyprai/loka/internal/vmagent"
)

func main() {
	agent, err := vmagent.ListenVsock()
	if err != nil {
		log.Fatalf("failed to start vmagent: %v", err)
	}
	defer agent.Close()

	log.Printf("loka-vmagent listening on vsock:%d", vmagent.VsockPort)
	if err := agent.Serve(); err != nil {
		log.Fatalf("vmagent serve error: %v", err)
	}
}
