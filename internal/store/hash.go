package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

const (
	// MaxTreeBytes bounds cumulative hashed bytes in a single dependency tree (100 MB).
	MaxTreeBytes int64 = 100 * 1024 * 1024
	// MaxTreeEntries bounds total file/directory entries in a single dependency tree.
	MaxTreeEntries = 5000
)

// ErrSymlinkNotAllowed is returned when a dependency tree contains a symbolic link.
var ErrSymlinkNotAllowed = errors.New("symbolic links are not allowed inside dependency trees")

type treeEntry struct {
	relPath string
	absPath string
	mode    os.FileMode
	isDir   bool
}

// HashTree computes an OS-independent canonical SHA-256 hash ("sha256:<hex>")
// over sorted POSIX relative paths, ignoring .git/, rejecting all symlinks,
// and enforcing cumulative byte and entry count caps.
func HashTree(root string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: %s", ErrSymlinkNotAllowed, root)
	}

	h := sha256.New()
	if !info.IsDir() {
		var total int64
		if err := hashRegularFile(h, filepath.Base(root), root, info.Mode(), &total); err != nil {
			return "", err
		}
		return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
	}

	var entries []treeEntry
	var entryCount int

	err = filepath.WalkDir(root, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if p == root {
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entryCount++
		if entryCount > MaxTreeEntries {
			return fmt.Errorf("dependency tree exceeds max entry limit (%d)", MaxTreeEntries)
		}

		linfo, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if linfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlinkNotAllowed, p)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		relPOSIX := filepath.ToSlash(rel)

		if linfo.IsDir() {
			// Only hash empty directories explicitly.
			sub, err := os.ReadDir(p)
			if err != nil {
				return err
			}
			if len(sub) == 0 {
				entries = append(entries, treeEntry{relPath: relPOSIX, absPath: p, isDir: true})
			}
			return nil
		}
		if !linfo.Mode().IsRegular() {
			return fmt.Errorf("unsupported non-regular file in dependency tree: %s", relPOSIX)
		}
		entries = append(entries, treeEntry{
			relPath: relPOSIX,
			absPath: p,
			mode:    linfo.Mode(),
		})
		return nil
	})
	if err != nil {
		return "", err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].relPath < entries[j].relPath
	})

	var totalBytes int64
	for _, e := range entries {
		if e.isDir {
			_, _ = io.WriteString(h, "D\x00"+e.relPath+"\x00")
			continue
		}
		if err := hashRegularFile(h, e.relPath, e.absPath, e.mode, &totalBytes); err != nil {
			return "", err
		}
	}

	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func hashRegularFile(h io.Writer, relPOSIX, absPath string, mode os.FileMode, totalBytes *int64) error {
	f, err := os.Open(absPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return err
	}
	sz := st.Size()
	if *totalBytes+sz > MaxTreeBytes {
		return fmt.Errorf("dependency tree exceeds max size limit (%d bytes)", MaxTreeBytes)
	}

	modeStr := "0644"
	if mode&0o111 != 0 {
		modeStr = "0755"
	}

	_, _ = io.WriteString(h, "F\x00"+relPOSIX+"\x00"+modeStr+"\x00"+strconv.FormatInt(sz, 10)+"\x00")

	remaining := MaxTreeBytes - *totalBytes
	n, err := io.Copy(h, io.LimitReader(f, remaining+1))
	if err != nil {
		return err
	}
	if n > remaining || n != sz {
		return fmt.Errorf("dependency tree file %s size mismatch or exceeds limit", relPOSIX)
	}
	*totalBytes += n
	return nil
}
