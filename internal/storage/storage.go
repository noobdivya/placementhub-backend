// Package storage abstracts where uploaded files live. Local disk today; an
// S3-compatible implementation can satisfy the same interface later.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var ErrNotFound = errors.New("storage: object not found")

type Storage interface {
	// Put stores r under key and returns the number of bytes written.
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// keyRE keeps keys to a flat, path-safe alphabet so a key can never escape the
// storage directory.
var keyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Local struct{ dir string }

func NewLocal(dir string) (*Local, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	return &Local{dir: abs}, nil
}

func (l *Local) path(key string) (string, error) {
	if !keyRE.MatchString(key) {
		return "", fmt.Errorf("storage: invalid key %q", key)
	}
	return filepath.Join(l.dir, key), nil
}

func (l *Local) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	p, err := l.path(key)
	if err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(l.dir, ".upload-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	n, err := io.Copy(tmp, r)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return 0, err
	}
	return n, nil
}

func (l *Local) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

func (l *Local) Delete(ctx context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
