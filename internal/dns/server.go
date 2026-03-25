package dns

import (
	"fmt"
	"net"

	"github.com/miekg/dns"
)

// Server is a minimal DNS server that answers *.{domain} queries with a
// fixed IP address. It is used to resolve .loka domains locally.
type Server struct {
	domain string // FQDN of the base domain, e.g. "loka."
	ip     net.IP // IP to return for A queries, e.g. 127.0.0.1
	addr   string // listen address, e.g. ":5453"
	server *dns.Server
}

// NewServer creates a DNS server that resolves *.domain to ip.
func NewServer(domain, ip, addr string) *Server {
	return &Server{
		domain: dns.Fqdn(domain),
		ip:     net.ParseIP(ip),
		addr:   addr,
	}
}

// Start begins listening for DNS queries in the background.
func (s *Server) Start() error {
	mux := dns.NewServeMux()
	mux.HandleFunc(s.domain, s.handleQuery)
	s.server = &dns.Server{
		Addr:    s.addr,
		Net:     "udp",
		Handler: mux,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.server.ListenAndServe()
	}()
	// Give the server a moment to fail on bind errors.
	select {
	case err := <-errCh:
		return fmt.Errorf("dns server failed: %w", err)
	default:
		return nil
	}
}

// Stop shuts down the DNS server.
func (s *Server) Stop() {
	if s.server != nil {
		s.server.Shutdown()
	}
}

// Addr returns the configured listen address.
func (s *Server) Addr() string {
	return s.addr
}

func (s *Server) handleQuery(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	for _, q := range r.Question {
		switch q.Qtype {
		case dns.TypeA:
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   s.ip,
			})
		case dns.TypeSOA:
			m.Answer = append(m.Answer, &dns.SOA{
				Hdr:    dns.RR_Header{Name: s.domain, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 3600},
				Ns:     "ns." + s.domain,
				Mbox:   "admin." + s.domain,
				Serial: 1,
			})
		}
	}
	w.WriteMsg(m)
}
