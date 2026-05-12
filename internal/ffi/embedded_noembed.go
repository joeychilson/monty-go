//go:build monty_noembed

package ffi

import "io/fs"

func readEmbeddedLibrary(string) ([]byte, error) {
	return nil, fs.ErrNotExist
}
