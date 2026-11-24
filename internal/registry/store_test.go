package registry

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestTouchDeletedDeviceFails(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	code, err := store.CreateInvite(ctx, "mac", nil)
	if err != nil {
		t.Fatal(err)
	}
	platform := Platform{OS: "darwin", Arch: "arm64", Shell: "/bin/zsh"}
	cred, err := store.ExchangeInvite(ctx, code, platform)
	if err != nil {
		t.Fatal(err)
	}
	devices, err := store.List(ctx)
	if err != nil || len(devices) != 1 {
		t.Fatalf("list: %+v, %v", devices, err)
	}
	id := devices[0].ID
	deletedID, err := store.Delete(ctx, cred.Name)
	if err != nil || deletedID != id {
		t.Fatalf("delete: id=%d err=%v", deletedID, err)
	}
	if err := store.Touch(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Touch on deleted device = %v, want ErrNotFound", err)
	}
}

func TestTouchExpiredDeviceFails(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	past := time.Now().Add(-time.Second)
	code, err := store.CreateInvite(ctx, "mac", &past)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := store.ExchangeInvite(ctx, code, Platform{OS: "darwin", Arch: "arm64", Shell: "/bin/zsh"})
	if err != nil {
		t.Fatal(err)
	}
	devices, err := store.List(ctx)
	if err != nil || len(devices) != 1 || devices[0].Name != cred.Name {
		t.Fatalf("list: %+v, %v", devices, err)
	}
	if err := store.Touch(ctx, devices[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Touch on expired device = %v, want ErrNotFound", err)
	}
}
