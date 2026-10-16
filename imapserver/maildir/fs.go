package maildir

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// These are the same byte-for-byte segment rules as cmd/imapcli. Keeping a
// private implementation avoids turning the backup command into a dependency.
func escapeSegment(s string) string {
	if s == "" {
		return "%"
	}
	reserved := strings.EqualFold(s, "cur") || strings.EqualFold(s, "new") || strings.EqualFold(s, "tmp")
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '%' || c == '/' || c < 32 || c == 127 || i == 0 && (c == '.' || reserved) {
			fmt.Fprintf(&out, "%%%02X", c)
		} else {
			out.WriteByte(c)
		}
	}
	return out.String()
}

func unescapeSegment(s string) (string, error) {
	if s == "%" {
		return "", nil
	}
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			out.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("maildir: invalid escaped segment")
		}
		b, err := hex.DecodeString(s[i+1 : i+3])
		if err != nil {
			return "", fmt.Errorf("maildir: invalid escaped segment")
		}
		out.WriteByte(b[0])
		i += 2
	}
	if escapeSegment(out.String()) != s {
		return "", fmt.Errorf("maildir: noncanonical escaped segment")
	}
	return out.String(), nil
}

func mailboxPath(name string) string {
	parts := strings.Split(name, "/")
	for i := range parts {
		parts[i] = escapeSegment(parts[i])
	}
	return filepath.Join(append([]string{"mailboxes"}, parts...)...)
}

func uniqueName() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func localPath(name string) bool {
	return filepath.IsLocal(name) && name != "." && !strings.ContainsRune(name, 0)
}

// The root owns structural directories. Only delivery into tmp/ and new/ may
// happen externally while open; structural directory renames are not allowed.
// Root bounds file opens even if an external delivery races its Lstat check.
func checkedPath(root *os.Root, name string) error {
	if name == "." {
		return nil
	}
	if !localPath(name) {
		return fmt.Errorf("maildir: unsafe storage path")
	}
	parts := strings.Split(filepath.Clean(name), string(filepath.Separator))
	for i := range parts {
		info, err := root.Lstat(filepath.Join(parts[:i+1]...))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("maildir: symbolic links are not supported")
		}
		if i+1 < len(parts) && !info.IsDir() {
			return fmt.Errorf("maildir: storage parent is not a directory")
		}
	}
	return nil
}

func mkdirAll(root *os.Root, path string) error {
	if err := checkedPath(root, path); err != nil {
		return err
	}
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	for i := range parts {
		name := filepath.Join(parts[:i+1]...)
		if err := root.Mkdir(name, 0700); err != nil {
			if !os.IsExist(err) {
				return err
			}
			info, statErr := root.Lstat(name)
			if statErr != nil {
				return statErr
			}
			if !info.IsDir() {
				return fmt.Errorf("maildir: storage parent is not a directory")
			}
		} else if err := syncDir(root, filepath.Dir(name)); err != nil {
			return err
		}
	}
	return nil
}

func syncDir(root *os.Root, path string) error {
	f, err := root.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func openRegular(root *os.Root, path string) (*os.File, error) {
	if err := checkedPath(root, path); err != nil {
		return nil, err
	}
	info, err := root.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("maildir: message is not a regular file")
	}
	f, err := root.OpenFile(path, readOpenFlags(), 0)
	if err != nil {
		return nil, err
	}
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("maildir: message is not a regular file")
	}
	return f, nil
}

func readFile(root *os.Root, path string) ([]byte, error) {
	f, err := openRegular(root, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 256<<20))
}

func atomicFile(root *os.Root, path string, data []byte) error {
	id, err := uniqueName()
	if err != nil {
		return err
	}
	tmp := path + ".tmp-" + id
	f, err := root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	_, writeErr := f.Write(data)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := renameFile(root, tmp, path); err != nil {
		return err
	}
	return syncDir(root, filepath.Dir(path))
}

func readDir(root *os.Root, path string) ([]fs.DirEntry, error) {
	if err := checkedPath(root, path); err != nil {
		return nil, err
	}
	f, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r contextReader) Read(buf []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(buf)
}
