//go:build !monty_noembed

package ffi

import "embed"

//go:embed lib/*/libmonty_ffi.*.gz
var embeddedLibraries embed.FS

func readEmbeddedLibrary(name string) ([]byte, error) {
	return embeddedLibraries.ReadFile(name)
}
