// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Import objects are immutable, checksummed and retained with task history. Job
// parameters contain only a relative digest, never a deployment-specific path.
func (s *Server) spoolImportPayload(ctx context.Context, content []byte) (string, error) {
	if e := ctx.Err(); e != nil {
		return "", e
	}
	if int64(len(content)) > s.Config.MaxBodyBytes {
		return "", errors.New("import content exceeds configured request limit")
	}
	dir := filepath.Join(s.DataDir, "import_payloads")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return "", e
	}
	root, e := os.OpenRoot(dir)
	if e != nil {
		return "", e
	}
	defer root.Close()
	hash := sha256.Sum256(content)
	name := fmt.Sprintf("%x.txt", hash)
	tmp := ".pending-" + authRandom()
	f, e := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return "", e
	}
	defer root.Remove(tmp)
	_, e = io.Copy(f, bytes.NewReader(content))
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return "", e
	}
	if closeErr != nil {
		return "", closeErr
	}
	if e = ctx.Err(); e != nil {
		return "", e
	}
	if e = root.Rename(tmp, name); e != nil {
		return "", e
	}
	d, e := root.Open(".")
	if e != nil {
		return "", e
	}
	e = d.Sync()
	d.Close()
	if e != nil {
		return "", e
	}
	return name, nil
}
func (s *Server) readImportPayload(ref string) ([]byte, error) {
	if len(ref) != 68 || !strings.HasSuffix(ref, ".txt") {
		return nil, errors.New("invalid import object reference")
	}
	expected, e := hex.DecodeString(strings.TrimSuffix(ref, ".txt"))
	if e != nil || len(expected) != 32 {
		return nil, errors.New("invalid import object hash")
	}
	root, e := os.OpenRoot(filepath.Join(s.DataDir, "import_payloads"))
	if e != nil {
		return nil, e
	}
	defer root.Close()
	f, e := root.Open(ref)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		return nil, errors.New("import object must be a regular file")
	}
	b, e := io.ReadAll(io.LimitReader(f, s.Config.MaxBodyBytes+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > s.Config.MaxBodyBytes {
		return nil, errors.New("import object exceeds configured size limit")
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != strings.TrimSuffix(ref, ".txt") {
		return nil, errors.New("import object checksum mismatch")
	}
	return b, nil
}
func (s *Server) libSpoolImports(ctx context.Context, p *libTaskParams) error {
	for i := range p.Items {
		item := &p.Items[i]
		if item.Content == "" {
			continue
		}
		ref, e := s.spoolImportPayload(ctx, []byte(item.Content))
		if e != nil {
			return e
		}
		item.ContentRef = ref
		item.Content = ""
	}
	return nil
}
func (s *Server) libReadImport(ref string) (string, error) {
	b, e := s.readImportPayload(ref)
	return string(b), e
}
