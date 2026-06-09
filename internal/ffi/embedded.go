package ffi

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
)

var errEmbeddedLibraryUnavailable = errors.New("monty: embedded Rust shared library unavailable")

const maxEmbeddedLibrarySize = 128 << 20

func embeddedLibraryPath(name string) (string, error) {
	assetPath := path.Join("lib", runtime.GOOS+"_"+runtime.GOARCH, name+".gz")
	data, err := readEmbeddedLibrary(assetPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errEmbeddedLibraryUnavailable
		}
		return "", fmt.Errorf("monty: read embedded Rust shared library %s: %w", assetPath, err)
	}
	if len(data) == 0 {
		return "", fmt.Errorf("monty: embedded Rust shared library %s is empty", assetPath)
	}

	sum := sha256.Sum256(data)
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		cacheRoot = os.TempDir()
	}
	cacheDir := filepath.Join(cacheRoot, "monty-go", runtime.GOOS+"_"+runtime.GOARCH+"-"+hex.EncodeToString(sum[:])[:16])
	target := filepath.Join(cacheDir, name)
	if info, err := os.Stat(target); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
		return target, nil
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", fmt.Errorf("monty: create embedded Rust shared library cache: %w", err)
	}

	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("monty: decompress embedded Rust shared library %s: %w", assetPath, err)
	}
	// The gzip CRC is validated when CopyN below reaches EOF, so any Close error
	// here is redundant.
	defer func() { _ = reader.Close() }()

	tmp, err := os.CreateTemp(cacheDir, name+".*.tmp")
	if err != nil {
		return "", fmt.Errorf("monty: create embedded Rust shared library temp file: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()

	n, err := io.CopyN(tmp, reader, maxEmbeddedLibrarySize+1)
	if err != nil && !errors.Is(err, io.EOF) {
		if closeErr := tmp.Close(); closeErr != nil {
			return "", fmt.Errorf("monty: write embedded Rust shared library: %w; close failed: %w", err, closeErr)
		}
		return "", fmt.Errorf("monty: write embedded Rust shared library: %w", err)
	}
	if n > maxEmbeddedLibrarySize {
		if closeErr := tmp.Close(); closeErr != nil {
			return "", fmt.Errorf("monty: embedded Rust shared library exceeds %d bytes; close failed: %w", maxEmbeddedLibrarySize, closeErr)
		}
		return "", fmt.Errorf("monty: embedded Rust shared library exceeds %d bytes", maxEmbeddedLibrarySize)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("monty: close embedded Rust shared library: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return "", fmt.Errorf("monty: install embedded Rust shared library: %w", err)
	}
	keep = true
	return target, nil
}
