package registry

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func openBenchStore(b *testing.B) *Store {
	b.Helper()
	store, err := Open(context.Background(), filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	return store
}

func benchPlatform() Platform {
	return Platform{OS: "darwin", Arch: "arm64", Shell: "/bin/zsh"}
}

func BenchmarkCreateAndExchangeInvite(b *testing.B) {
	store := openBenchStore(b)
	ctx := context.Background()
	platform := benchPlatform()
	expiry := time.Now().Add(time.Hour)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		code, err := store.CreateInvite(ctx, "bench-node", &expiry)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := store.ExchangeInvite(ctx, code, platform); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSameNameCollisionJoin(b *testing.B) {
	store := openBenchStore(b)
	ctx := context.Background()
	for i := 0; i < 32; i++ {
		joinBenchDevice(ctx, b, store, "mac")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		joinBenchDevice(ctx, b, store, "mac")
	}
}

func BenchmarkListDevices(b *testing.B) {
	store := openBenchStore(b)
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		joinBenchDevice(ctx, b, store, fmt.Sprintf("node-%d", i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := store.List(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func joinBenchDevice(ctx context.Context, b *testing.B, store *Store, name string) {
	b.Helper()
	code, err := store.CreateInvite(ctx, name, nil)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := store.ExchangeInvite(ctx, code, benchPlatform()); err != nil {
		b.Fatal(err)
	}
}
