// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/danmaku"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

const (
	predownloadPoolsKey              = "anidan.predownload.pools"
	predownloadPoolLimit             = 16
	predownloadBudgetBytes     int64 = 256 << 20
	predownloadBudgetJSONLimit       = 32 << 10
	predownloadBudgetTimeout         = 3 * time.Second
)

var (
	errPredownloadPoolConflict = errors.New("episode already has a predownload reservation")
	errPredownloadPoolCapacity = errors.New("predownload storage budget exhausted")
	errPredownloadPoolState    = errors.New("invalid predownload bookkeeping; reservations retained")
	errPredownloadPoolIdentity = errors.New("predownload identity changed or disappeared; reservation retained")
	errPredownloadPoolToken    = errors.New("predownload reservation token changed")
	errPredownloadPoolFile     = errors.New("predownload XML unavailable, changed, or exceeds the byte limit; reservation retained")
)

// Identity contains only a fingerprint of the exact download identity. Paths and
// provider credentials are deliberately absent so SQL copy/restore can relocate
// the library without weakening identity checks or leaking them through status.
type predownloadPoolEntry struct {
	EpisodeID     int64  `json:"episodeId"`
	SourceID      int64  `json:"sourceId"`
	Identity      string `json:"identity"`
	ReservationID string `json:"reservationId"`
	ReservedBytes int64  `json:"reservedBytes"`
	State         string `json:"state"`
}

type predownloadPoolBook struct {
	Version int                    `json:"version"`
	Entries []predownloadPoolEntry `json:"entries"`
}

type predownloadPoolBudgetStats struct {
	Count          int    `json:"count"`
	ReservedCount  int    `json:"reservedCount"`
	PublishedCount int    `json:"publishedCount"`
	ReservedBytes  int64  `json:"reservedBytes"`
	PublishedBytes int64  `json:"publishedBytes"`
	TotalBytes     int64  `json:"totalBytes"`
	UnknownEntries int    `json:"unknownEntries"`
	Degraded       bool   `json:"degraded"`
	Diagnostic     string `json:"diagnostic,omitempty"`
}

func predownloadIdentity(ep, src store.Row) string {
	values := []any{ep["id"], ep["source_id"], ep["provider_episode_id"], ep["episode_index"], src["id"], src["anime_id"], src["provider_name"], src["media_id"], src["source_order"]}
	// Rows read from Store have canonical int64/string values. Preserve null vs
	// empty provider IDs in the fingerprint as well.
	raw, _ := json.Marshal(values)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func predownloadValidHex(v string, bytes int) bool {
	if len(v) != bytes*2 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

// JSON's default decoder accepts duplicate keys (including case aliases), which
// makes a damaged record ambiguous. Reject them before trusting any byte charge.
func predownloadUniqueJSON(d *json.Decoder, depth int) error {
	if depth > 4 {
		return errPredownloadPoolState
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errPredownloadPoolState
			}
			name = strings.ToLower(name)
			if keys[name] {
				return errPredownloadPoolState
			}
			keys[name] = true
			if err = predownloadUniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := predownloadUniqueJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return errPredownloadPoolState
	}
	_, err = d.Token()
	return err
}

func (b predownloadPoolBook) validate() error {
	if b.Version != 1 || b.Entries == nil || len(b.Entries) > predownloadPoolLimit {
		return errPredownloadPoolState
	}
	ids := make(map[int64]bool, len(b.Entries))
	tokens := make(map[string]bool, len(b.Entries))
	var total int64
	for _, e := range b.Entries {
		if e.EpisodeID <= 0 || e.SourceID <= 0 || ids[e.EpisodeID] || tokens[e.ReservationID] || !predownloadValidHex(e.Identity, 32) || !predownloadValidHex(e.ReservationID, 16) {
			return errPredownloadPoolState
		}
		maxBytes := danmaku.DefaultParseOptions().MaxBytes
		if e.ReservedBytes <= 0 || (e.State != "reserved" && e.State != "published") || (e.State == "reserved" && e.ReservedBytes != 3*maxBytes) || (e.State == "published" && e.ReservedBytes > 2*maxBytes) {
			return errPredownloadPoolState
		}
		ids[e.EpisodeID], tokens[e.ReservationID] = true, true
		total += e.ReservedBytes
		if total > predownloadBudgetBytes {
			return errPredownloadPoolState
		}
	}
	return nil
}

// Read only a bounded config value from SQL. Checking len after Store.Get would
// already have transferred an arbitrarily large malformed value into memory.
func (s *Server) readPredownloadBook(ctx context.Context, tx *sql.Tx) (predownloadPoolBook, bool, error) {
	b := predownloadPoolBook{Version: 1, Entries: []predownloadPoolEntry{}}
	length := "OCTET_LENGTH(config_value)"
	if s.Store.Dialect == "sqlite" {
		length = "LENGTH(CAST(config_value AS BLOB))"
	}
	q := "SELECT CASE WHEN " + length + "<=? THEN config_value ELSE NULL END," + length + " FROM config WHERE config_key=?"
	var raw sql.NullString
	var size int64
	err := tx.QueryRowContext(ctx, s.Store.Rebind(q), predownloadBudgetJSONLimit, predownloadPoolsKey).Scan(&raw, &size)
	if errors.Is(err, sql.ErrNoRows) {
		return b, false, nil
	}
	if err != nil {
		return b, false, err
	}
	if size > predownloadBudgetJSONLimit || size < 1 || !raw.Valid || len(raw.String) > predownloadBudgetJSONLimit {
		return b, true, errPredownloadPoolState
	}
	if err = predownloadUniqueJSON(json.NewDecoder(bytes.NewBufferString(raw.String)), 0); err != nil {
		return b, true, errPredownloadPoolState
	}
	b = predownloadPoolBook{}
	d := json.NewDecoder(bytes.NewBufferString(raw.String))
	d.DisallowUnknownFields()
	if err = d.Decode(&b); err != nil {
		return b, true, errPredownloadPoolState
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return b, true, errPredownloadPoolState
	}
	return b, true, b.validate()
}

func (s *Server) writePredownloadBook(ctx context.Context, tx *sql.Tx, b predownloadPoolBook, exists bool) error {
	if err := b.validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(b)
	if err != nil || len(raw) > predownloadBudgetJSONLimit {
		return errPredownloadPoolState
	}
	if exists {
		_, err = tx.ExecContext(ctx, s.Store.Rebind("UPDATE config SET config_value=? WHERE config_key=?"), string(raw), predownloadPoolsKey)
	} else {
		_, err = s.Store.InsertTx(ctx, tx, "config", store.Row{"config_key": predownloadPoolsKey, "config_value": string(raw), "description": "Bounded unplayed predownload reservations; never an eviction list"})
	}
	return err
}

func (s *Server) predownloadRows(ctx context.Context, tx *sql.Tx, id int64) (store.Row, store.Row, error) {
	// Do not select full episode rows: source_url is unconstrained text and is
	// irrelevant to the download identity. SUBSTR also bounds corrupt SQLite
	// strings before transfer (SQLite does not enforce VARCHAR lengths).
	var episodeID, sourceID, index int64
	var providerID, path sql.NullString
	err := tx.QueryRowContext(ctx, s.Store.Rebind("SELECT id,source_id,episode_index,SUBSTR(provider_episode_id,1,501),SUBSTR(danmaku_file_path,1,1025) FROM episode WHERE id=?"), id).Scan(&episodeID, &sourceID, &index, &providerID, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, errPredownloadPoolIdentity
	}
	if err != nil {
		return nil, nil, err
	}
	if utf8.RuneCountInString(providerID.String) > 500 || utf8.RuneCountInString(path.String) > 1024 {
		return nil, nil, errPredownloadPoolIdentity
	}
	ep := store.Row{"id": episodeID, "source_id": sourceID, "episode_index": index, "provider_episode_id": nil, "danmaku_file_path": nil}
	if providerID.Valid {
		ep["provider_episode_id"] = providerID.String
	}
	if path.Valid {
		ep["danmaku_file_path"] = path.String
	}
	var animeID, sourceOrder int64
	var providerName, mediaID string
	err = tx.QueryRowContext(ctx, s.Store.Rebind("SELECT anime_id,source_order,SUBSTR(provider_name,1,501),SUBSTR(media_id,1,256) FROM anime_sources WHERE id=?"), sourceID).Scan(&animeID, &sourceOrder, &providerName, &mediaID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, errPredownloadPoolIdentity
	}
	if err != nil {
		return nil, nil, err
	}
	if utf8.RuneCountInString(providerName) > 500 || utf8.RuneCountInString(mediaID) > 255 {
		return nil, nil, errPredownloadPoolIdentity
	}
	src := store.Row{"id": sourceID, "anime_id": animeID, "source_order": sourceOrder, "provider_name": providerName, "media_id": mediaID}
	return ep, src, nil
}

// All operations belong to this application's single manager instance. The
// dedicated mutex serializes read/modify/write; the short SQL transaction is
// crash-atomic. A crash after reservation and before settlement charges three
// parser maxima indefinitely: XML object, custom target, and rollback copy. The
// speculative publisher must reject a rollback source larger than one maximum.
func (s *Server) reservePredownloadPool(ctx context.Context, ep store.Row) (string, error) {
	s.predownloadBudgetMu.Lock()
	defer s.predownloadBudgetMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, predownloadBudgetTimeout)
	defer cancel()
	tx, err := s.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	b, exists, err := s.readPredownloadBook(ctx, tx)
	if err != nil {
		return "", err
	}
	id := number(ep["id"])
	var total int64
	for _, entry := range b.Entries {
		if entry.EpisodeID == id {
			return "", errPredownloadPoolConflict
		}
		total += entry.ReservedBytes
	}
	maxBytes := 3 * danmaku.DefaultParseOptions().MaxBytes
	if len(b.Entries) >= predownloadPoolLimit || maxBytes <= 0 || maxBytes > predownloadBudgetBytes-total {
		return "", errPredownloadPoolCapacity
	}
	current, src, err := s.predownloadRows(ctx, tx, id)
	if err != nil {
		return "", err
	}
	if predownloadIdentity(current, src) != predownloadIdentity(ep, src) {
		return "", errPredownloadPoolIdentity
	}
	token := randomID()
	b.Entries = append(b.Entries, predownloadPoolEntry{EpisodeID: id, SourceID: number(current["source_id"]), Identity: predownloadIdentity(current, src), ReservationID: token, ReservedBytes: maxBytes, State: "reserved"})
	if err = s.writePredownloadBook(ctx, tx, b, exists); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return token, nil
}

func (s *Server) settlePredownloadPool(ctx context.Context, ep store.Row, reservationID string) error {
	// Use the publication/backup lock ordering: file lock, budget lock, SQL.
	s.fileMu.RLock()
	defer s.fileMu.RUnlock()
	s.predownloadBudgetMu.Lock()
	defer s.predownloadBudgetMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, predownloadBudgetTimeout)
	defer cancel()
	tx, err := s.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	b, exists, err := s.readPredownloadBook(ctx, tx)
	if err != nil {
		return err
	}
	index := -1
	for i := range b.Entries {
		if b.Entries[i].EpisodeID == number(ep["id"]) {
			index = i
			break
		}
	}
	// Real playback may consume the entry while shared download is finishing.
	// Never recreate it, and never touch a newer reservation with an old token.
	if index < 0 {
		return nil
	}
	entry := &b.Entries[index]
	if reservationID == "" || entry.ReservationID != reservationID {
		return errPredownloadPoolToken
	}
	current, src, err := s.predownloadRows(ctx, tx, entry.EpisodeID)
	if err != nil {
		return err
	}
	if entry.Identity != predownloadIdentity(current, src) || entry.Identity != predownloadIdentity(ep, src) {
		return errPredownloadPoolIdentity
	}
	if s.fileFault.Load() || str(current["danmaku_file_path"]) == "" || str(current["danmaku_file_path"]) != str(ep["danmaku_file_path"]) {
		return errPredownloadPoolFile
	}
	f, err := s.resolveDanmaku(str(current["danmaku_file_path"]))
	if err != nil {
		return errPredownloadPoolFile
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > danmaku.DefaultParseOptions().MaxBytes {
		return errPredownloadPoolFile
	}
	// Repeated settlement cannot lower a previously published charge. Other
	// library edits are outside this proactive allocation, and never an excuse
	// to reclaim unknown bytes silently.
	if entry.State == "published" {
		return nil
	}
	// Conservatively charge both the immutable object and a possible custom
	// target; default single-object paths are intentionally charged the same.
	entry.State, entry.ReservedBytes = "published", 2*info.Size()
	if err = s.writePredownloadBook(ctx, tx, b, exists); err != nil {
		return err
	}
	return tx.Commit()
}

// A nonempty token is only for an abandoned fetch known not to have published.
// The caller must establish that fact. An empty token consumes bookkeeping for
// actual foreground playback. Neither path removes files from the library.
func (s *Server) releasePredownloadPool(ctx context.Context, episodeID int64, reservationID string) error {
	s.predownloadBudgetMu.Lock()
	defer s.predownloadBudgetMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, predownloadBudgetTimeout)
	defer cancel()
	tx, err := s.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	b, exists, err := s.readPredownloadBook(ctx, tx)
	if err != nil {
		return err
	}
	for i, entry := range b.Entries {
		if entry.EpisodeID != episodeID {
			continue
		}
		if reservationID != "" {
			if entry.ReservationID != reservationID {
				return errPredownloadPoolToken
			}
			if entry.State != "reserved" {
				return errPredownloadPoolState
			}
		} else {
			ep, src, err := s.predownloadRows(ctx, tx, episodeID)
			if err != nil {
				return err
			}
			if entry.Identity != predownloadIdentity(ep, src) {
				return errPredownloadPoolIdentity
			}
		}
		b.Entries = append(b.Entries[:i], b.Entries[i+1:]...)
		if err = s.writePredownloadBook(ctx, tx, b, exists); err != nil {
			return err
		}
		return tx.Commit()
	}
	return nil
}

// Deleted/rebound identities stay charged: absence of a row does not prove its
// immutable XML was removed. Status gives a bounded, path-free diagnostic so an
// operator can investigate rather than allowing repeated orphan allocations.
func (s *Server) predownloadPoolStats(ctx context.Context) (predownloadPoolBudgetStats, error) {
	s.predownloadBudgetMu.Lock()
	defer s.predownloadBudgetMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, predownloadBudgetTimeout)
	defer cancel()
	var stats predownloadPoolBudgetStats
	fail := func(err error) (predownloadPoolBudgetStats, error) {
		stats.Degraded = true
		stats.Diagnostic = "Predownload bookkeeping requires review; existing reservations remain charged"
		return stats, err
	}
	tx, err := s.Store.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	b, _, err := s.readPredownloadBook(ctx, tx)
	if err != nil {
		return fail(err)
	}
	stats.Count = len(b.Entries)
	for _, entry := range b.Entries {
		stats.TotalBytes += entry.ReservedBytes
		if entry.State == "reserved" {
			stats.ReservedCount++
			stats.ReservedBytes += entry.ReservedBytes
		} else {
			stats.PublishedCount++
			stats.PublishedBytes += entry.ReservedBytes
		}
	}
	for _, entry := range b.Entries {
		ep, src, err := s.predownloadRows(ctx, tx, entry.EpisodeID)
		if err != nil {
			if !errors.Is(err, errPredownloadPoolIdentity) {
				return fail(err)
			}
			stats.UnknownEntries++
		} else if entry.Identity != predownloadIdentity(ep, src) {
			stats.UnknownEntries++
		}
	}
	if stats.UnknownEntries != 0 {
		return fail(errPredownloadPoolIdentity)
	}
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	return stats, nil
}
