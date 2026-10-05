// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/AniBakaBaka/AniDan/internal/store"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var immutableObjectName = regexp.MustCompile(`^[0-9]+-[a-f0-9]{64}\.xml$`)

type fileWriteJournal struct {
	ID           string `json:"id"`
	EpisodeID    int64  `json:"episodeId"`
	Target       string `json:"target"`
	Object       string `json:"object"`
	Backup       string `json:"backup,omitempty"`
	HadTarget    bool   `json:"hadTarget"`
	ObjectSHA256 string `json:"objectSha256"`
	BackupSHA256 string `json:"backupSha256,omitempty"`
}

func (s *Server) commentTarget(ctx context.Context, ep, src store.Row, object string) (string, error) {
	if boolean(s.setting(ctx, "customDanmakuPathEnabled", "false")) {
		a, e := s.Store.Get(ctx, "anime", src["anime_id"])
		if e != nil {
			return "", e
		}
		template := s.setting(ctx, "customDanmakuPathTemplate", "/app/config/danmaku/${animeId}/${episodeId}")
		rendered, e := s.renderStorageTemplate(ctx, template, a, src, ep)
		if e != nil {
			return "", e
		}
		root, _, target, e := s.storageTarget(rendered)
		if e != nil {
			return "", e
		}
		root.Close()
		return target, nil
	}
	old := str(ep["danmaku_file_path"])
	if old != "" && !immutableObjectName.MatchString(filepath.Base(s.normalizeStoredPath(old))) {
		root, _, target, e := s.storageTarget(old)
		if e == nil {
			root.Close()
			return target, nil
		}
	}
	return object, nil
}
func (s *Server) writeJournal(j fileWriteJournal) error {
	root, e := os.OpenRoot(s.DataDir)
	if e != nil {
		return e
	}
	defer root.Close()
	if e = root.MkdirAll("write_journal", 0700); e != nil {
		return e
	}
	b, e := json.Marshal(j)
	if e != nil {
		return e
	}
	f, e := root.OpenFile(filepath.Join("write_journal", j.ID+".json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	dir, e := root.Open("write_journal")
	if e != nil {
		return e
	}
	defer dir.Close()
	if e = dir.Sync(); e != nil {
		return e
	}
	return syncAnchoredDirectories(root, ".")
}
func (s *Server) copyToManaged(ctx context.Context, input *os.File, relative string) error {
	return s.copyToManagedLimit(ctx, input, relative, 256<<20)
}
func (s *Server) copyToManagedLimit(ctx context.Context, input *os.File, relative string, limit int64) error {
	root, e := os.OpenRoot(s.DataDir)
	if e != nil {
		return e
	}
	defer root.Close()
	if e = root.MkdirAll(filepath.Dir(relative), 0700); e != nil {
		return e
	}
	f, e := root.OpenFile(relative, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	n, e := io.Copy(f, contextReader{ctx, io.LimitReader(input, limit)})
	if e != nil {
		f.Close()
		return e
	}
	var extra [1]byte
	more, readErr := input.Read(extra[:])
	if n > limit || more != 0 || (readErr != nil && readErr != io.EOF) {
		f.Close()
		return errors.New("file journal input exceeds its byte limit or changed during copying")
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return syncAnchoredDirectories(root, filepath.Dir(relative))
}
func (s *Server) publishObject(ctx context.Context, object, target string) error {
	src, e := s.resolveDanmaku(object)
	if e != nil {
		return e
	}
	defer src.Close()
	root, rel, _, e := s.storageTarget(target)
	if e != nil {
		return e
	}
	defer root.Close()
	if e = root.MkdirAll(filepath.Dir(rel), 0700); e != nil {
		return e
	}
	temp := filepath.Join(filepath.Dir(rel), ".publish-"+randomID()+".tmp")
	f, e := root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer root.Remove(temp)
	if _, e = io.Copy(f, contextReader{ctx, src}); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	before, _ := root.Stat(rel)
	if e = root.Rename(temp, rel); e != nil {
		return e
	}
	after, _ := root.Stat(rel)
	s.invalidateCommentFile(target, before, after)
	dir, e := root.Open(filepath.Dir(rel))
	if e != nil {
		return e
	}
	defer dir.Close()
	if e = dir.Sync(); e != nil {
		return e
	}
	return syncAnchoredDirectories(root, filepath.Dir(rel))
}
func (s *Server) finishJournal(ctx context.Context, j fileWriteJournal) error {
	root, e := os.OpenRoot(s.DataDir)
	if e != nil {
		return e
	}
	defer root.Close()
	if e = root.Remove(filepath.Join("write_journal", j.ID+".json")); e != nil && !os.IsNotExist(e) {
		return e
	}
	// The unlink must be durable before the SQL commit marker may disappear.
	if e = syncAnchoredDirectories(root, "write_journal"); e != nil {
		return e
	}
	return s.Store.Delete(ctx, "config", "anidan.filewrite."+j.ID)
}
func (s *Server) rollbackJournal(ctx context.Context, j fileWriteJournal) error {
	state, e := s.journalTargetState(ctx, j)
	if e != nil {
		return e
	}
	if j.HadTarget {
		if state == "old" {
			return nil
		}
		if state != "new" {
			return errors.New("original target disappeared after interruption; manual recovery required")
		}
		return s.publishObject(ctx, j.Backup, j.Target)
	}
	if state == "missing" {
		return nil
	}
	if state != "new" {
		return errors.New("target changed after interruption")
	}
	root, rel, _, e := s.storageTarget(j.Target)
	if e != nil {
		return e
	}
	defer root.Close()
	before, _ := root.Stat(rel)
	if e = root.Remove(rel); e != nil {
		return e
	}
	s.invalidateCommentFile(j.Target, before, nil)
	return syncAnchoredDirectories(root, filepath.Dir(rel))
}

// commitCommentObject reuses legacy/custom filenames with an fsynced rollback
// journal and a commit marker in the same SQL transaction as the episode update.
// Readers and backup snapshots share fileMu, avoiding a DB/file split snapshot.
func (s *Server) commitCommentObject(ctx context.Context, ep, src store.Row, object string, count int, preserveSmaller bool) error {
	_, err := s.commitCommentObjectOutcome(ctx, ep, src, object, count, preserveSmaller)
	return err
}

func (s *Server) commitCommentObjectOutcome(ctx context.Context, ep, src store.Row, object string, count int, preserveSmaller bool) (commentPublicationOutcome, error) {
	out := commentPublicationOutcome{FetchedCount: count, StagingStarted: object != ""}
	plan, planned := ctx.Value(importPublicationPlanKey{}).(importPublicationPlan)
	var target string
	var e error
	if !planned {
		target, e = s.commentTarget(ctx, ep, src, object)
		if e != nil {
			return out, e
		}
	}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	if s.fileFault.Load() {
		return out, errors.New("storage recovery required before further writes")
	}
	if planned {
		// Resolve after acquiring the publication lock: a queued saver must
		// observe policy changes made while another write held that lock.
		// Once checked, this attempt uses its captured target; config writes
		// themselves are not serialized by fileMu and can affect later work.
		if boolean(s.setting(ctx, "customDanmakuPathEnabled", "false")) {
			return out, errors.New("publication path policy changed during import; reload and retry")
		}
		target, e = s.commentTarget(ctx, ep, src, object)
		if e != nil {
			return out, e
		}
		expected := plan.target
		if expected == "" {
			expected = object
		}
		if target != expected {
			return out, errors.New("publication destination changed during import; reload and retry")
		}
	}
	tx, e := s.Store.DB.BeginTx(ctx, nil)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	if e = s.validateDownloadIdentity(ctx, tx, ep, src); e != nil {
		return out, e
	}
	var oldPath sql.NullString
	if e = tx.QueryRowContext(ctx, s.Store.Rebind("SELECT danmaku_file_path FROM episode WHERE id=?"), ep["id"]).Scan(&oldPath); e != nil {
		return out, e
	}
	if oldPath.String != str(ep["danmaku_file_path"]) {
		return out, errors.New("episode changed during fetch; retry instead of overwriting a newer path")
	}
	var pool commentPoolState
	if before, ok := ctx.Value(commentPoolSnapshotKey{}).(commentPoolState); ok {
		pool, e = s.recheckCommentPoolLocked(ctx, ep, before)
	} else {
		pool, e = s.inspectCommentPoolLocked(ctx, ep)
	}
	if e != nil {
		return out, e
	}
	out.PoolCount, out.PoolCountKnown = pool.count, true
	if preserveSmaller && pool.missing && count == 0 {
		out.Disposition = commentEmptySkipped
		return out, nil
	}
	if preserveSmaller && !pool.missing && int64(count) < pool.count {
		out.Disposition = commentSmallerRetained
		return out, nil
	}
	var journal *fileWriteJournal
	if target != object {
		// A custom template may point somewhere other than the episode's
		// current pool. Validate those bytes too before any backup or replace.
		if immutableObjectName.MatchString(filepath.Base(target)) {
			return out, errors.New("custom destination collides with an immutable comment filename")
		}
		targetPool := pool
		if str(ep["danmaku_file_path"]) == "" || s.normalizeStoredPath(str(ep["danmaku_file_path"])) != target {
			var targetErr error
			targetPool, targetErr = s.inspectCommentPoolLocked(ctx, store.Row{"danmaku_file_path": target})
			if targetErr != nil {
				return out, targetErr
			}
		}
		root, rel, _, e := s.storageTarget(target)
		if e != nil {
			return out, e
		}
		info, statErr := root.Lstat(rel)
		if statErr != nil && !os.IsNotExist(statErr) {
			root.Close()
			return out, statErr
		}
		if statErr == nil && !info.Mode().IsRegular() {
			root.Close()
			return out, errors.New("custom target must be a regular file")
		}
		if targetPool.missing != (statErr != nil) || (statErr == nil && (!os.SameFile(targetPool.info, info) || targetPool.info.Size() != info.Size() || !targetPool.info.ModTime().Equal(info.ModTime()))) {
			root.Close()
			return out, errStoredPoolChanged
		}
		if ctx.Value(predownloadPublicationKey{}) == true && statErr == nil && info.Size() > 64<<20 {
			root.Close()
			return out, errors.New("predownload custom rollback source exceeds 64MiB")
		}
		if e = s.ensureUnsharedTarget(ctx, tx, number(ep["id"]), target); e != nil {
			root.Close()
			return out, e
		}
		j := fileWriteJournal{ID: randomID(), EpisodeID: number(ep["id"]), Target: target, Object: object, HadTarget: statErr == nil}
		if j.HadTarget {
			input, e := root.OpenFile(rel, regularReadFlags, 0)
			if e != nil {
				root.Close()
				return out, e
			}
			opened, openErr := input.Stat()
			if openErr != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || info.Size() != opened.Size() || !info.ModTime().Equal(opened.ModTime()) {
				input.Close()
				root.Close()
				return out, errStoredPoolChanged
			}
			j.Backup = filepath.Join(s.DataDir, "rollback", "files", j.ID+".xml")
			limit := int64(256 << 20)
			if ctx.Value(predownloadPublicationKey{}) == true {
				limit = 64 << 20
			}
			e = s.copyToManagedLimit(ctx, input, filepath.Join("rollback", "files", j.ID+".xml"), limit)
			input.Close()
			if e != nil {
				root.Close()
				return out, e
			}
		}
		root.Close()
		j.ObjectSHA256, e = s.hashDanmakuFile(ctx, j.Object)
		if e != nil {
			return out, e
		}
		if j.HadTarget {
			j.BackupSHA256, e = s.hashDanmakuFile(ctx, j.Backup)
			if e != nil {
				return out, e
			}
			if j.BackupSHA256 != hex.EncodeToString(targetPool.digest[:]) {
				return out, errStoredPoolChanged
			}
			current, e := s.hashDanmakuFile(ctx, j.Target)
			if e != nil || current != j.BackupSHA256 {
				return out, errors.New("custom target changed while preparing rollback backup")
			}
		}
		if e = s.writeJournal(j); e != nil {
			return out, e
		}
		journal = &j
		out.TargetWriteStarted = true
		if e = s.publishObject(ctx, object, target); e != nil {
			return out, s.abortFileWrite(tx, j, e)
		}
	}
	if _, e = tx.ExecContext(ctx, s.Store.Rebind("UPDATE episode SET danmaku_file_path=?,comment_count=?,fetched_at=? WHERE id=?"), target, count, s.now(), ep["id"]); e != nil {
		if journal != nil {
			return out, s.abortFileWrite(tx, *journal, e)
		}
		return out, e
	}
	if journal != nil {
		if _, e = s.Store.InsertTx(ctx, tx, "config", store.Row{"config_key": "anidan.filewrite." + journal.ID, "config_value": "committed"}); e != nil {
			return out, s.abortFileWrite(tx, *journal, e)
		}
	}
	if e = tx.Commit(); e != nil {
		out.CommitUncertain = true
		out.PoolCountKnown = false
		if journal != nil {
			s.fileFault.Store(true)
			s.cancel()
			return out, fmt.Errorf("commit outcome uncertain; journal retained for restart recovery: %w", e)
		}
		return out, e
	}
	out.Committed = true
	out.PoolCount, out.PoolCountKnown = int64(count), true
	out.Disposition = commentPublished
	if journal != nil {
		if e = s.finishJournal(ctx, *journal); e != nil {
			s.fileFault.Store(true)
			s.cancel()
			return out, fmt.Errorf("data committed but journal cleanup failed; restart recovery required: %w", e)
		}
	}
	return out, nil
}
func (s *Server) recoverFileWrites(ctx context.Context) error {
	root, e := os.OpenRoot(s.DataDir)
	if e != nil {
		return e
	}
	defer root.Close()
	dir, e := root.Open("write_journal")
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	entries, e := dir.ReadDir(10001)
	dir.Close()
	if e != nil && e != io.EOF {
		return e
	}
	if len(entries) > 10000 {
		return errors.New("too many pending filesystem journals")
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return errors.New("invalid filesystem journal entry")
		}
		b, e := root.ReadFile(filepath.Join("write_journal", entry.Name()))
		if e != nil {
			return e
		}
		if len(b) > 16384 {
			return errors.New("filesystem journal too large")
		}
		var j fileWriteJournal
		if e = json.Unmarshal(b, &j); e != nil {
			return e
		}
		if j.ID+".json" != entry.Name() || len(j.ID) != 32 {
			return errors.New("invalid filesystem journal identity")
		}
		if j.HadTarget && j.Backup != filepath.Join(s.DataDir, "rollback", "files", j.ID+".xml") {
			return errors.New("invalid journal backup path")
		}
		rel, e := filepath.Rel(filepath.Join(s.DataDir, "danmaku"), j.Object)
		if e != nil || !filepath.IsLocal(rel) || !immutableObjectName.MatchString(filepath.Base(j.Object)) {
			return errors.New("invalid journal object path")
		}
		if _, e = s.Store.Get(ctx, "config", "anidan.filewrite."+j.ID); e == nil {
			state, err := s.journalTargetState(ctx, j)
			if err != nil {
				return err
			}
			if state == "missing" {
				return errors.New("committed target disappeared; manual recovery required")
			}
			if state == "old" {
				if e = s.publishObject(ctx, j.Object, j.Target); e != nil {
					return fmt.Errorf("recover committed file write: %w", e)
				}
			}
		} else if errors.Is(e, sql.ErrNoRows) {
			if e = s.rollbackJournal(ctx, j); e != nil {
				return fmt.Errorf("recover interrupted file write: %w", e)
			}
		} else {
			return e
		}
		if e = s.finishJournal(ctx, j); e != nil {
			return e
		}
	}
	return nil
}

func (s *Server) abortFileWrite(tx *sql.Tx, j fileWriteJournal, cause error) error {
	_ = tx.Rollback()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e := s.rollbackJournal(ctx, j); e != nil {
		s.fileFault.Store(true)
		s.cancel()
		return errors.Join(cause, fmt.Errorf("rollback failed; restart recovery required: %w", e))
	}
	if e := s.finishJournal(ctx, j); e != nil {
		s.fileFault.Store(true)
		s.cancel()
		return errors.Join(cause, e)
	}
	return cause
}

// syncAnchoredDirectories persists each newly created ancestor, never escaping root.
func syncAnchoredDirectories(root *os.Root, relative string) error {
	relative = filepath.Clean(relative)
	for {
		dir, e := root.Open(relative)
		if e != nil {
			return e
		}
		e = dir.Sync()
		dir.Close()
		if e != nil {
			return e
		}
		if relative == "." {
			return nil
		}
		relative = filepath.Dir(relative)
	}
}
func (s *Server) hashDanmakuFile(ctx context.Context, path string) (string, error) {
	f, e := s.resolveDanmaku(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return "", e
	}
	if !st.Mode().IsRegular() {
		return "", errors.New("journal object is not a regular file")
	}
	h := sha256.New()
	n, e := io.Copy(h, contextReader{ctx, io.LimitReader(f, (256<<20)+1)})
	if e != nil {
		return "", e
	}
	if n > 256<<20 {
		return "", errors.New("journal file exceeds256MiB")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func (s *Server) journalTargetState(ctx context.Context, j fileWriteJournal) (string, error) {
	if len(j.ObjectSHA256) != 64 {
		return "", errors.New("journal lacks expected object checksum; manual review required")
	}
	object, e := s.hashDanmakuFile(ctx, j.Object)
	if e != nil || object != j.ObjectSHA256 {
		return "", errors.New("immutable journal object checksum mismatch")
	}
	if j.HadTarget {
		if len(j.BackupSHA256) != 64 {
			return "", errors.New("journal lacks backup checksum")
		}
		backup, e := s.hashDanmakuFile(ctx, j.Backup)
		if e != nil || backup != j.BackupSHA256 {
			return "", errors.New("journal backup checksum mismatch")
		}
	}
	target, e := s.hashDanmakuFile(ctx, j.Target)
	if os.IsNotExist(e) {
		return "missing", nil
	}
	if e != nil {
		return "", e
	}
	if target == j.ObjectSHA256 {
		return "new", nil
	}
	if j.HadTarget && target == j.BackupSHA256 {
		return "old", nil
	}
	return "", errors.New("target was externally changed after interruption; refusing to overwrite or delete it")
}

// pathIdentity compares anchored directory/file identity plus missing suffixes,
// catching legacy aliases, in-root symlinks and hard links without reading outside
// explicitly allowed roots. Failure to validate another reference fails closed.
func (s *Server) pathIdentity(path string) (os.FileInfo, string, error) {
	normalized := s.normalizeStoredPath(path)
	roots := append([]string{s.DataDir}, s.Config.ReadRoots...)
	roots = append(roots, s.Config.WriteRoots...)
	for _, base := range roots {
		base, e := filepath.Abs(base)
		if e != nil {
			continue
		}
		rel, e := filepath.Rel(base, normalized)
		if e != nil || !filepath.IsLocal(rel) {
			continue
		}
		root, e := os.OpenRoot(base)
		if e != nil {
			return nil, "", e
		}
		defer root.Close()
		suffix := ""
		for {
			before, e := root.Stat(rel)
			if e == nil && ((!before.Mode().IsRegular() && !before.IsDir()) || (suffix != "" && !before.IsDir())) {
				return nil, "", errors.New("stored file reference is nonregular")
			}
			if e == nil {
				var f *os.File
				f, e = root.OpenFile(rel, regularReadFlags, 0)
				if e == nil {
					st, statErr := f.Stat()
					f.Close()
					if statErr != nil {
						return nil, "", statErr
					}
					if !os.SameFile(before, st) || ((!st.Mode().IsRegular() && !st.IsDir()) || (suffix != "" && !st.IsDir())) {
						return nil, "", errors.New("stored file reference changed while opening")
					}
					return st, suffix, nil
				}
			}
			if !os.IsNotExist(e) {
				return nil, "", e
			}
			// An unresolved symlink can become a live alias of the target as
			// soon as publication creates it. Do not treat it as a lexical
			// missing suffix and accidentally activate another episode's pool.
			if before != nil || !genuinePoolAbsence(root, rel) {
				return nil, "", errors.New("stored file reference disappeared or contains an unresolved link")
			}
			if rel == "." {
				return nil, "", e
			}
			suffix = filepath.Join(filepath.Base(rel), suffix)
			rel = filepath.Dir(rel)
		}
	}
	return nil, "", errors.New("stored file reference is outside configured roots")
}
func (s *Server) ensureUnsharedTarget(ctx context.Context, tx *sql.Tx, episodeID int64, target string) error {
	targetInfo, targetTail, e := s.pathIdentity(target)
	if e != nil {
		return e
	}
	rows, e := tx.QueryContext(ctx, s.Store.Rebind("SELECT id,danmaku_file_path FROM episode WHERE id<>? AND danmaku_file_path IS NOT NULL"), episodeID)
	if e != nil {
		return e
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var path string
		if e = rows.Scan(&id, &path); e != nil {
			return e
		}
		if path == "" {
			continue
		}
		info, tail, e := s.pathIdentity(path)
		if e != nil {
			return fmt.Errorf("cannot safely validate file reference for episode %d: %w", id, e)
		}
		if tail == targetTail && os.SameFile(info, targetInfo) {
			return fmt.Errorf("custom target is shared by episode %d through a filesystem/path alias", id)
		}
	}
	return rows.Err()
}
