package hub

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/12bitsD/device-link/internal/registry"
)

func BenchmarkHandleDevicesList(b *testing.B) {
	ctx := context.Background()
	store, err := registry.Open(ctx, filepath.Join(b.TempDir(), "hub.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	token, err := store.MintControllerToken(ctx)
	if err != nil {
		b.Fatal(err)
	}
	platform := registry.Platform{OS: "linux", Arch: "arm64", Shell: "/bin/sh"}
	for i := 0; i < 50; i++ {
		code, inviteErr := store.CreateInvite(ctx, fmt.Sprintf("node-%d", i), nil)
		if inviteErr != nil {
			b.Fatal(inviteErr)
		}
		if _, joinErr := store.ExchangeInvite(ctx, code, platform); joinErr != nil {
			b.Fatal(joinErr)
		}
	}
	server := &Server{store: store, live: make(map[int64]*nodeSession)}
	handler := server.routes()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/devices", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}
