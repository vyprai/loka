package api

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/vyprai/loka/internal/store"
)

// resolveSessionID resolves a session ID or name to the actual UUID.
// Tries in order: UUID parse, direct ID lookup, name lookup.
func (s *Server) resolveSessionID(ctx context.Context, idOrName string) (string, error) {
	// If it's a valid UUID, use directly
	if _, err := uuid.Parse(idOrName); err == nil {
		return idOrName, nil
	}
	// Try as a raw ID (non-UUID formats like test IDs)
	if sess, err := s.sessionManager.Get(ctx, idOrName); err == nil && sess != nil {
		return sess.ID, nil
	}
	// Try as a name
	sessions, err := s.sessionManager.List(ctx, store.SessionFilter{Name: &idOrName, Limit: 1})
	if err != nil || len(sessions) == 0 {
		return "", fmt.Errorf("session %q not found", idOrName)
	}
	return sessions[0].ID, nil
}

// resolveServiceID resolves a service ID or name to the actual UUID.
// Tries in order: UUID parse, direct ID lookup, name lookup.
func (s *Server) resolveServiceID(ctx context.Context, idOrName string) (string, error) {
	// If it's a valid UUID, use directly
	if _, err := uuid.Parse(idOrName); err == nil {
		return idOrName, nil
	}
	// Try as a raw ID (non-UUID formats like test IDs)
	if svc, err := s.serviceManager.Get(ctx, idOrName); err == nil && svc != nil {
		return svc.ID, nil
	}
	// Try as a name
	services, _, err := s.serviceManager.List(ctx, store.ServiceFilter{Name: &idOrName, Limit: 1})
	if err != nil || len(services) == 0 {
		return "", fmt.Errorf("service %q not found", idOrName)
	}
	return services[0].ID, nil
}
