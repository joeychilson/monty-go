package monty

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// fsMount serves read-only Path operations under a virtual prefix from an
// fs.FS (configured with WithFS).
type fsMount struct {
	virtualPath string
	fsys        fs.FS
}

func newFSMount(virtualPath string, fsys fs.FS) fsMount {
	return fsMount{virtualPath: cleanVirtualPath(virtualPath), fsys: fsys}
}

// fsPath maps a virtual Python path onto an fs.FS path, reporting false when
// the path is outside this mount.
func (m fsMount) fsPath(virtual string) (string, bool) {
	cleaned := path.Clean(virtual)
	if cleaned == m.virtualPath {
		return ".", true
	}
	prefix := m.virtualPath
	if prefix != "/" {
		prefix += "/"
	}
	if !strings.HasPrefix(cleaned, prefix) {
		return "", false
	}
	relative := strings.TrimPrefix(cleaned, prefix)
	if relative == "" {
		return ".", true
	}
	if !fs.ValidPath(relative) {
		return "", false
	}
	return relative, true
}

// handle answers one filesystem OS call. handled is false when the path is
// outside this mount; write operations inside the mount raise PermissionError.
func (m fsMount) handle(fn OSFunction, args []Value, _ map[string]Value) (Value, bool, error) {
	target, ok := pathArgument(args[0])
	if !ok {
		return Value{}, false, nil
	}
	name, ok := m.fsPath(target)
	if !ok {
		return Value{}, false, nil
	}
	switch fn {
	case OSPathExists:
		_, err := fs.Stat(m.fsys, name)
		return Bool(err == nil), true, nil
	case OSPathIsFile:
		info, err := fs.Stat(m.fsys, name)
		return Bool(err == nil && !info.IsDir()), true, nil
	case OSPathIsDir:
		info, err := fs.Stat(m.fsys, name)
		return Bool(err == nil && info.IsDir()), true, nil
	case OSPathIsSymlink:
		// fs.FS has no symlink traversal; entries are never reported as links.
		return Bool(false), true, nil
	case OSPathReadText:
		data, err := fs.ReadFile(m.fsys, name)
		if err != nil {
			return Value{}, true, fsError(err, target)
		}
		return Str(string(data)), true, nil
	case OSPathReadBytes:
		data, err := fs.ReadFile(m.fsys, name)
		if err != nil {
			return Value{}, true, fsError(err, target)
		}
		return Bytes(data), true, nil
	case OSPathStat:
		info, err := fs.Stat(m.fsys, name)
		if err != nil {
			return Value{}, true, fsError(err, target)
		}
		return StatOf(info).MontyValue(), true, nil
	case OSPathIterDir:
		entries, err := fs.ReadDir(m.fsys, name)
		if err != nil {
			return Value{}, true, fsError(err, target)
		}
		items := make([]Value, len(entries))
		base := path.Clean(target)
		for i, entry := range entries {
			items[i] = Path(path.Join(base, entry.Name())).MontyValue()
		}
		return List(items...), true, nil
	case OSPathResolve, OSPathAbsolute:
		return Path(path.Clean(target)).MontyValue(), true, nil
	case OSPathWriteText, OSPathWriteBytes, OSPathMkdir, OSPathUnlink, OSPathRmdir, OSPathRename:
		return Value{}, true, &Exception{
			Type:    "PermissionError",
			Message: fmt.Sprintf("%s is read-only: %q", fn, target),
		}
	default:
		return Value{}, false, nil
	}
}

// pathArgument extracts the string form of a Path-like first argument.
func pathArgument(value Value) (string, bool) {
	switch value.Kind() {
	case PathKind, StringKind:
		return value.Str(), true
	default:
		return "", false
	}
}

// fsError maps fs errors onto the matching Python exception types.
func fsError(err error, target string) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &Exception{Type: "FileNotFoundError", Message: fmt.Sprintf("No such file or directory: %q", target)}
	case errors.Is(err, fs.ErrPermission):
		return &Exception{Type: "PermissionError", Message: fmt.Sprintf("Permission denied: %q", target)}
	default:
		return &Exception{Type: "OSError", Message: err.Error()}
	}
}
