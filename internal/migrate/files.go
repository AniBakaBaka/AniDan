// SPDX-License-Identifier: AGPL-3.0-only
package migrate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/store"
)

type RootMapping struct {
	From string `json:"from"`
	To   string `json:"to"`
}
type FileReceipt struct {
	Source        string   `json:"source"`
	OriginalPaths []string `json:"original_paths"`
	Destination   string   `json:"destination"`
	SHA256        string   `json:"sha256"`
	Size          int64    `json:"size"`
	root          *os.Root
	relative      string
}
type mappedRoot struct {
	legacy, actual string
	index          int
	handle         *os.Root
}
type fileMapper struct {
	roots  []mappedRoot
	target string
	files  map[string]*FileReceipt
	limit  int
}

func newFileMapper(roots []RootMapping, target string) (*fileMapper, error) {
	m := &fileMapper{target: target, files: map[string]*FileReceipt{}, limit: 100000}
	success := false
	defer func() {
		if !success {
			m.Close()
		}
	}()
	seen := map[string]bool{}
	for i, r := range roots {
		from, e := legacyPath(r.From)
		if e != nil {
			return nil, e
		}
		if from == "." || from == "/" || from == "" {
			return nil, errors.New("map an explicit legacy file root, not filesystem root")
		}
		key := from
		if len(key) > 1 && key[1] == ':' {
			key = strings.ToLower(key)
		}
		if seen[key] {
			return nil, fmt.Errorf("duplicate legacy root %s", from)
		}
		seen[key] = true
		actual, e := filepath.Abs(r.To)
		if e != nil {
			return nil, e
		}
		actual, e = filepath.EvalSymlinks(actual)
		if e != nil {
			return nil, e
		}
		st, e := os.Stat(actual)
		if e != nil {
			return nil, e
		}
		if !st.IsDir() {
			return nil, fmt.Errorf("source root is not a directory: %s", actual)
		}
		h, e := openDirectoryRoot(actual)
		if e != nil {
			m.Close()
			return nil, e
		}
		m.roots = append(m.roots, mappedRoot{from, actual, i, h})
	}
	sort.SliceStable(m.roots, func(i, j int) bool { return len(m.roots[i].legacy) > len(m.roots[j].legacy) })
	success = true
	return m, nil
}
func legacyPath(p string) (string, error) {
	if strings.ContainsRune(p, 0) {
		return "", errors.New("NUL in path")
	}
	p = strings.ReplaceAll(p, "\\", "/")
	for _, x := range strings.Split(p, "/") {
		if x == ".." {
			return "", errors.New("path traversal rejected")
		}
	}
	return strings.TrimSuffix(path.Clean(p), "/"), nil
}
func prefixMatch(p, root string) bool {
	if len(root) > 1 && root[1] == ':' {
		p = strings.ToLower(p)
		root = strings.ToLower(root)
	}
	return p == root || strings.HasPrefix(p, root+"/")
}
func (m *fileMapper) mapRow(ctx context.Context, table string, row store.Row) error {
	fields := map[string][]string{"episode": {"danmaku_file_path"}, "anime": {"local_image_path", "image_url"}, "local_danmaku_items": {"file_path", "nfo_path", "poster_url"}, "media_items": {"poster_url"}, "external_calendar_item": {"image_url"}}
	for _, field := range fields[table] {
		v, ok := row[field].(string)
		if !ok || strings.TrimSpace(v) == "" {
			continue
		}
		isURLField := field == "image_url" || field == "poster_url"
		if isURLField && (strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "data:") || strings.HasPrefix(v, "/api/ui/media-servers/") || strings.HasPrefix(v, "/api/ui/local-items/")) {
			continue
		}
		p, e := legacyPath(v)
		if e != nil {
			return e
		}
		publicImage := strings.HasPrefix(p, "/data/images/")
		if publicImage {
			p = "/app/config/image/" + strings.TrimPrefix(p, "/data/images/")
		}
		var chosen *mappedRoot
		for i := range m.roots {
			if prefixMatch(p, m.roots[i].legacy) {
				chosen = &m.roots[i]
				break
			}
		}
		if chosen == nil {
			return fmt.Errorf("no root mapping for %s.%s path %q", table, field, v)
		}
		rel := strings.TrimPrefix(p[len(chosen.legacy):], "/")
		if rel == "" {
			return fmt.Errorf("referenced path is a root directory: %q", v)
		}
		actual := filepath.Join(chosen.actual, filepath.FromSlash(rel))
		resolved, e := filepath.EvalSymlinks(actual)
		if e != nil {
			return fmt.Errorf("missing/unreadable source file %q: %w", v, e)
		}
		if !within(chosen.actual, resolved) {
			return fmt.Errorf("source symlink escapes mapped root: %q", v)
		}
		dest := path.Join("files", fmt.Sprintf("root-%d", chosen.index), rel)
		if idx := strings.Index(p, "/config/image/"); idx >= 0 {
			dest = path.Join("image", p[idx+len("/config/image/"):])
		} else if strings.HasSuffix(chosen.legacy, "/image") {
			dest = path.Join("image", rel)
		} else if idx := strings.Index(p, "/config/danmaku/"); idx >= 0 {
			dest = path.Join("danmaku", p[idx+len("/config/danmaku/"):])
		}
		if _, exists := m.files[dest]; !exists && len(m.files) >= m.limit {
			return fmt.Errorf("migration file manifest exceeds limit %d; raise MaxFiles explicitly for a larger installation", m.limit)
		}
		sourceRel, e := filepath.Rel(chosen.actual, resolved)
		if e != nil {
			return e
		}
		sourceFile, e := openRegularSource(ctx, chosen.handle, sourceRel)
		if e != nil {
			return e
		}
		sum, size, e := hashOpenFile(ctx, sourceFile)
		sourceFile.Close()
		if e != nil {
			return e
		}
		if old, ok := m.files[dest]; ok {
			if old.SHA256 != sum || old.Size != size {
				return fmt.Errorf("different source files map to %s", dest)
			}
			found := false
			for _, o := range old.OriginalPaths {
				if o == v {
					found = true
				}
			}
			if !found {
				old.OriginalPaths = append(old.OriginalPaths, v)
			}
		} else {
			m.files[dest] = &FileReceipt{Source: resolved, OriginalPaths: []string{v}, Destination: dest, SHA256: sum, Size: size, root: chosen.handle, relative: sourceRel}
		}
		if !publicImage {
			row[field] = filepath.Join(m.target, filepath.FromSlash(dest))
		}
	}
	return nil
}
func (m *fileMapper) receipts() []FileReceipt {
	out := make([]FileReceipt, 0, len(m.files))
	for _, k := range sortedKeys(m.files) {
		f := *m.files[k]
		sort.Strings(f.OriginalPaths)
		out = append(out, f)
	}
	return out
}
func within(root, p string) bool {
	rel, e := filepath.Rel(root, p)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func (m *fileMapper) Close() {
	for _, r := range m.roots {
		if r.handle != nil {
			r.handle.Close()
		}
	}
}
func hashFile(ctx context.Context, p string) (string, int64, error) {
	f, e := openRegularPath(ctx, p)
	if e != nil {
		return "", 0, e
	}
	defer f.Close()
	return hashOpenFile(ctx, f)
}
func hashDestinationPath(ctx context.Context, p string) (string, int64, error) {
	root, e := openDirectoryRoot(filepath.Dir(p))
	if e != nil {
		return "", 0, e
	}
	defer root.Close()
	f, e := openRegularDestination(ctx, root, filepath.Base(p))
	if e != nil {
		return "", 0, e
	}
	defer f.Close()
	return hashOpenFile(ctx, f)
}
func hashOpenFile(ctx context.Context, f *os.File) (string, int64, error) {
	p := f.Name()
	st, e := f.Stat()
	if e != nil {
		return "", 0, e
	}
	if !st.Mode().IsRegular() {
		return "", 0, fmt.Errorf("not a regular file: %s", p)
	}
	h := sha256.New()
	n, e := copyContext(ctx, h, f)
	if e != nil {
		return "", 0, e
	}
	end, e := f.Stat()
	if e != nil {
		return "", 0, e
	}
	if n != st.Size() || end.Size() != st.Size() || !end.ModTime().Equal(st.ModTime()) {
		return "", 0, fmt.Errorf("source changed while reading: %s", p)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
func copyContext(ctx context.Context, w io.Writer, r io.Reader) (int64, error) {
	buf := make([]byte, 128<<10)
	var total int64
	for {
		if e := ctx.Err(); e != nil {
			return total, e
		}
		n, re := r.Read(buf)
		if n > 0 {
			nw, we := w.Write(buf[:n])
			total += int64(nw)
			if we != nil {
				return total, we
			}
			if nw != n {
				return total, io.ErrShortWrite
			}
		}
		if re == io.EOF {
			return total, nil
		}
		if re != nil {
			return total, re
		}
	}
}
func rejectSymlink(p string) error {
	st, e := os.Lstat(p)
	if e != nil {
		return e
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("symlink destination rejected: %s", p)
	}
	return nil
}
func rejectSymlinkIfExists(p string) error {
	e := rejectSymlink(p)
	if os.IsNotExist(e) {
		return nil
	}
	return e
}
func safeDirectories(root, dir string) error {
	if !within(root, dir) {
		return errors.New("destination escaped staging directory")
	}
	rel, e := filepath.Rel(root, dir)
	if e != nil {
		return e
	}
	p := root
	if e = rejectSymlink(p); e != nil {
		return e
	}
	for _, piece := range strings.Split(rel, string(filepath.Separator)) {
		if piece == "." || piece == "" {
			continue
		}
		p = filepath.Join(p, piece)
		if e = os.Mkdir(p, 0700); e != nil && !os.IsExist(e) {
			return e
		}
		if e = rejectSymlink(p); e != nil {
			return e
		}
	}
	return nil
}
func copyFiles(ctx context.Context, files []FileReceipt, stage string) error {
	root, e := openDirectoryRoot(stage)
	if e != nil {
		return e
	}
	defer root.Close()
	for _, f := range files {
		if e = ctx.Err(); e != nil {
			return e
		}
		dest := filepath.FromSlash(f.Destination)
		if !filepath.IsLocal(dest) {
			return errors.New("invalid file destination")
		}
		if e = root.MkdirAll(filepath.Dir(dest), 0700); e != nil {
			return e
		}
		if existing, err := openRegularDestination(ctx, root, dest); err == nil {
			sum, size, err := hashOpenFile(ctx, existing)
			existing.Close()
			if err != nil {
				return err
			}
			if sum != f.SHA256 || size != f.Size {
				return fmt.Errorf("resume destination checksum differs: %s", dest)
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if f.root == nil {
			return errors.New("source file has no anchored root handle")
		}
		src, e := openRegularSource(ctx, f.root, f.relative)
		if e != nil {
			return e
		}
		tmpName := filepath.Join(filepath.Dir(dest), ".anidan-copy-"+rand.Text())
		tmp, e := root.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			src.Close()
			return e
		}
		hash := sha256.New()
		n, e := copyContext(ctx, io.MultiWriter(tmp, hash), src)
		src.Close()
		if e == nil && (n != f.Size || hex.EncodeToString(hash.Sum(nil)) != f.SHA256) {
			e = fmt.Errorf("source changed since preflight: %s", f.Source)
		}
		if e == nil {
			e = tmp.Sync()
		}
		ce := tmp.Close()
		if e == nil {
			e = ce
		}
		if e != nil {
			root.Remove(tmpName)
			return e
		}
		if e = root.Link(tmpName, dest); e != nil {
			root.Remove(tmpName)
			return fmt.Errorf("exclusive file publication %s: %w", dest, e)
		}
		if e = root.Remove(tmpName); e != nil {
			return e
		}
		parent, e := openDirectoryWithin(root, filepath.Dir(dest))
		if e != nil {
			return e
		}
		dir, e := parent.Open(".")
		parent.Close()
		if e != nil {
			return e
		}
		e = dir.Sync()
		dir.Close()
		if e != nil {
			return e
		}
	}
	return nil
}
