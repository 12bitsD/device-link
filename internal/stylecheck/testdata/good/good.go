package good

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// bits:allow(IMPORT): registers the pure-Go sqlite driver with database/sql
import _ "modernc.org/sqlite"

var ErrNotFound = errors.New("not found")

type Server struct {
	addr string
}

func (s *Server) Serve(ctx context.Context, items []string) error {
	if len(items) == 0 {
		return ErrNotFound
	}
	for _, item := range items {
		if err := s.process(ctx, item); err != nil {
			return fmt.Errorf("process item: %w", err)
		}
	}
	return nil
}

func (s *Server) process(ctx context.Context, item string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if item == "" {
		return ErrNotFound
	}
	if len(item) > 10 {
		return nil
	}
	return nil
}

// bits:allow(BCS004): net/http middleware signature requires a boolean passthrough in this fixture
func callbackAdapter(w http.ResponseWriter, r *http.Request, passthrough bool) {
	w.WriteHeader(http.StatusOK)
	_ = r
	_ = passthrough
}

// TODO(stage2): replace linear scan once device count exceeds 1000
func lookup(items []string, target string) int {
	for i, item := range items {
		if item == target {
			return i
		}
	}
	return -1
}
