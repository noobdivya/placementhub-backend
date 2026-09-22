package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalRoundTripAndDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	n, err := s.Put(ctx, "abc-123.pdf", bytes.NewReader([]byte("hello")))
	if err != nil || n != 5 {
		t.Fatalf("Put = %d, %v", n, err)
	}
	r, err := s.Open(ctx, "abc-123.pdf")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "hello" {
		t.Errorf("read %q", got)
	}
	if err := s.Delete(ctx, "abc-123.pdf"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(ctx, "abc-123.pdf"); !errors.Is(err, ErrNotFound) {
		t.Errorf("open after delete: %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "abc-123.pdf"); err != nil {
		t.Errorf("deleting twice should be a no-op: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestKeysCannotEscapeTheDirectory(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	s, _ := NewLocal(dir)
	ctx := context.Background()
	for _, key := range []string{"../outside.txt", `..\outside.txt`, "/etc/passwd", "a/b", "", ".hidden", `C:\x`, "a\x00b", "sub/../../outside.txt"} {
		if _, err := s.Put(ctx, key, bytes.NewReader([]byte("x"))); err == nil {
			t.Errorf("Put accepted key %q", key)
		}
		if _, err := s.Open(ctx, key); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Open accepted key %q (%v)", key, err)
		}
		if err := s.Delete(ctx, key); err == nil {
			t.Errorf("Delete accepted key %q", key)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("a file was written outside the storage directory")
	}
}
