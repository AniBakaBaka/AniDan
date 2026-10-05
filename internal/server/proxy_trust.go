// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/proxyroute"
)

const accelerateTrustFile = ".anidan-accelerate-trust.json"
const acceleratePendingFile = ".anidan-accelerate-pending.json"
const accelerateTrustConfirmation = "TRUST_GATEWAY_WITH_ORIGIN_CREDENTIALS"
const accelerateApprovalKey = "anidan.proxy.accelerate_approval_id"

// This local record is deliberately absent from SQL migration/configuration and
// backup manifests. Importing proxy selections cannot grant gateway access.
type accelerateTrustRecord struct {
	Version    int    `json:"version"`
	DataRoot   string `json:"dataRoot"`
	Gateway    string `json:"gateway"`
	ApprovalID string `json:"approvalId"`
	ApprovedAt string `json:"approvedAt"`
}

type proxyRoutingState struct {
	mode       string
	proxy      string
	gateway    string
	approvalID string
	blocked    string
}

func (s *Server) proxyChangeFault(phase string) error {
	if s.proxyTrustFault != nil {
		return s.proxyTrustFault(phase)
	}
	return nil
}

func proxyPendingValid(root *os.Root) error {
	st, err := root.Lstat(acceleratePendingFile)
	if err != nil || !st.Mode().IsRegular() || st.Size() > 1024 {
		return errors.New("pending gateway change is not a bounded regular record")
	}
	f, err := root.OpenFile(acceleratePendingFile, regularReadFlags, 0)
	if err != nil {
		return errors.New("pending gateway change cannot be read")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(st, opened) {
		return errors.New("pending gateway change changed")
	}
	b, err := io.ReadAll(io.LimitReader(f, 1025))
	if err != nil || len(b) > 1024 {
		return errors.New("pending gateway change exceeds limits")
	}
	if len(b) == 0 {
		return nil
	} // interrupted first creation; still blocks startup
	var value struct {
		Version int    `json:"version"`
		State   string `json:"state"`
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&value) != nil || d.Decode(new(any)) != io.EOF || value.Version != 1 || value.State != "pending" {
		return errors.New("pending gateway change requires local review")
	}
	return nil
}

// Persist this fence before SQL or approval-file changes. Its mere existence
// blocks startup; a matching old/new approval pair cannot bypass uncertainty.
func (s *Server) beginProxyChange(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(s.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.OpenFile(acceleratePendingFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		if err = proxyPendingValid(root); err != nil {
			return err
		}
		f, err = root.OpenFile(acceleratePendingFile, regularReadFlags, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		if err = f.Sync(); err != nil {
			return err
		}
		return syncAnchoredDirectories(root, ".")
	}
	if err != nil {
		return err
	}
	_, err = f.Write([]byte("{\"version\":1,\"state\":\"pending\"}\n"))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	} // retain even a partial fence
	return syncAnchoredDirectories(root, ".")
}

func (s *Server) finishProxyChange(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(s.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	if err = proxyPendingValid(root); err != nil {
		return err
	}
	if err = s.proxyChangeFault("before-clear-pending"); err != nil {
		return err
	}
	if err = root.Remove(acceleratePendingFile); err != nil {
		return err
	}
	if err = syncAnchoredDirectories(root, "."); err == nil {
		err = s.proxyChangeFault("after-clear-pending")
	}
	if err != nil {
		// Restore a conservative fence if final unlink durability is uncertain.
		// Unrecoverable filesystem failure still requires operator storage review.
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return errors.Join(err, s.beginProxyChange(cleanup))
	}
	return nil
}

func (s *Server) readAccelerateTrust() (*accelerateTrustRecord, error) {
	root, err := os.OpenRoot(s.DataDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	before, err := root.Lstat(accelerateTrustFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil || !before.Mode().IsRegular() || before.Size() > 8192 {
		return nil, errors.New("local gateway approval record is invalid")
	}
	f, err := root.OpenFile(accelerateTrustFile, regularReadFlags, 0)
	if err != nil {
		return nil, errors.New("local gateway approval record cannot be read")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.New("local gateway approval record changed")
	}
	b, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(b) > 8192 {
		return nil, errors.New("local gateway approval record exceeds its limit")
	}
	after, err := root.Lstat(accelerateTrustFile)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return nil, errors.New("local gateway approval record changed")
	}
	var record accelerateTrustRecord
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || record.Version != 1 || record.DataRoot != filepath.Clean(s.DataDir) {
		return nil, errors.New("local gateway approval record does not match this installation")
	}
	if record.Gateway == "" && record.ApprovalID == "" {
		return nil, nil
	}
	canonical, err := proxyroute.CanonicalGateway(record.Gateway)
	if err != nil || canonical != record.Gateway || len(record.ApprovalID) != 32 {
		return nil, errors.New("local gateway approval record is invalid")
	}
	if decoded, err := hex.DecodeString(record.ApprovalID); err != nil || len(decoded) != 16 {
		return nil, errors.New("local gateway approval identity is invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, record.ApprovedAt); err != nil {
		return nil, errors.New("local gateway approval timestamp is invalid")
	}
	return &record, nil
}

// Caller owns providerConfigMu. SQL configuration and this file are not a
// distributed transaction. Any uncertain file write revokes the runtime approval;
// a subsequent explicit save is required before requests can use the gateway.
func (s *Server) writeAccelerateTrust(ctx context.Context, record *accelerateTrustRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := os.OpenRoot(s.DataDir)
	if err != nil {
		return err
	}
	defer root.Close()
	old, err := root.Lstat(accelerateTrustFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("local gateway approval cannot be inspected")
	}
	if err == nil && !old.Mode().IsRegular() {
		return errors.New("local gateway approval is not a regular file; review it locally")
	}
	if record == nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		record = &accelerateTrustRecord{Version: 1, DataRoot: filepath.Clean(s.DataDir)}
	}
	b, err := json.Marshal(record)
	if err != nil || len(b) > 8192 {
		return errors.New("invalid local gateway approval")
	}
	temporary := ".anidan-accelerate-trust-" + randomID()
	f, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("local gateway approval cannot be staged")
	}
	defer root.Remove(temporary)
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return errors.New("local gateway approval was not committed")
	}
	// Never replace an unexpected nonregular entry discovered at publication.
	if now, e := root.Lstat(accelerateTrustFile); e == nil && !now.Mode().IsRegular() || e != nil && !errors.Is(e, os.ErrNotExist) {
		return errors.New("local gateway approval changed before publication")
	}
	if err = root.Rename(temporary, accelerateTrustFile); err != nil {
		return errors.New("local gateway approval publication failed")
	}
	if err = syncAnchoredDirectories(root, "."); err != nil {
		return errors.New("local gateway approval durability is uncertain")
	}
	return nil
}

func (s *Server) proxySettings(ctx context.Context) (map[string]string, error) {
	// Map matches back to the requested spelling. A legacy MySQL collation may
	// equate case/accent variants even though Go map keys do not.
	keys := []string{"proxyMode", "proxyEnabled", "proxyUrl", "accelerateProxyUrl", "proxySslVerify", accelerateApprovalKey}
	queries := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys)*2)
	for _, key := range keys {
		queries = append(queries, "SELECT ? AS requested_key,config_value FROM config WHERE config_key=?")
		args = append(args, key, key)
	}
	rows, err := s.Store.DB.QueryContext(ctx, s.Store.Rebind(strings.Join(queries, " UNION ALL ")), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := map[string]string{}
	for rows.Next() {
		var key string
		var value sql.NullString
		if err = rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		if _, exists := values[key]; exists {
			return nil, errors.New("ambiguous proxy configuration")
		}
		values[key] = value.String
	}
	return values, rows.Err()
}

func (s *Server) readProxyRouting(ctx context.Context) (*proxyRoutingState, error) {
	values, err := s.proxySettings(ctx)
	if err != nil {
		return nil, errors.New("proxy configuration cannot be read")
	}
	state := &proxyRoutingState{mode: values["proxyMode"]}
	if state.mode == "" {
		state.mode = "none"
	}
	if state.mode == "none" && boolean(values["proxyEnabled"]) {
		state.mode = "http_socks"
	}
	switch state.mode {
	case "none":
		return state, nil
	case "http_socks":
		state.proxy = values["proxyUrl"]
		if state.proxy == "" {
			return nil, errors.New("proxy is enabled but proxyUrl is empty")
		}
		if err = proxyroute.Validate(proxyroute.Config{ProxyURL: state.proxy}); err != nil {
			return nil, errors.New("HTTP/SOCKS proxy configuration is invalid")
		}
		return state, nil
	case "accelerate":
		root, e := os.OpenRoot(s.DataDir)
		if e != nil {
			state.blocked = "approval_invalid"
			return state, nil
		}
		_, pendingErr := root.Lstat(acceleratePendingFile)
		root.Close()
		if !errors.Is(pendingErr, os.ErrNotExist) {
			state.blocked = "approval_pending"
			return state, nil
		}
		gateway, err := proxyroute.CanonicalGateway(values["accelerateProxyUrl"])
		if err != nil {
			state.blocked = "gateway_invalid"
			return state, nil
		}
		state.gateway = gateway
		record, err := s.readAccelerateTrust()
		if err != nil {
			state.blocked = "approval_invalid"
			return state, nil
		}
		if record == nil || record.Gateway != gateway || record.ApprovalID != values[accelerateApprovalKey] {
			state.blocked = "approval_required"
			return state, nil
		}
		state.approvalID = record.ApprovalID
		return state, nil
	default:
		return nil, errors.New("unknown proxy mode")
	}
}

func (s *Server) reloadProxyRouting(ctx context.Context) error {
	state, err := s.readProxyRouting(ctx)
	if err != nil {
		s.proxyRouting.Store(&proxyRoutingState{mode: "accelerate", blocked: "configuration_unavailable"})
		return err
	}
	s.proxyRouting.Store(state)
	return nil
}

func (s *Server) selectedProxyRouting(use bool) proxyroute.Config {
	if !use {
		return proxyroute.Config{}
	}
	state := s.proxyRouting.Load()
	if state == nil {
		return proxyroute.Config{Blocked: true}
	}
	if state.mode == "none" {
		return proxyroute.Config{}
	}
	if state.mode == "http_socks" {
		return proxyroute.Config{ProxyURL: state.proxy}
	}
	if state.blocked != "" || state.approvalID == "" {
		return proxyroute.Config{Blocked: true, AccelerateURL: state.gateway}
	}
	return proxyroute.Config{AccelerateURL: state.gateway, AuthorizationIdentity: state.approvalID, Authorize: s.accelerateAuthorizer(state.gateway, state.approvalID)}
}

// Also used on detached preflight clients: a prospective approval cannot send
// until its exact identity has actually become the current committed policy.
func (s *Server) accelerateAuthorizer(gateway, approvalID string) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current := s.proxyRouting.Load()
		if approvalID == "" || current == nil || current.mode != "accelerate" || current.blocked != "" || current.gateway != gateway || current.approvalID != approvalID {
			return errors.New("gateway approval was revoked or changed")
		}
		return nil
	}
}

func proxyConfigurationKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "proxymode", "proxyenabled", "proxyurl", "proxysslverify", "accelerateproxyurl":
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(key)), "anidan.proxy.")
}

func validPublicConfigKey(key string) bool {
	if len(key) == 0 || len(key) > 500 {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 33 || key[i] > 126 {
			return false
		}
	}
	return true
}

// Preserve the old masked HTTP/SOCKS URL setter without allowing the generic
// configuration endpoint to approve or alter a selected accelerate gateway.
func (s *Server) legacyHTTPProxyURLPut(w http.ResponseWriter, r *http.Request) {
	s.providerConfigMu.Lock()
	defer s.providerConfigMu.Unlock()
	var in map[string]any
	if e := readJSON(r, &in); e != nil {
		httpError(w, 400, "Invalid proxy URL setting")
		return
	}
	value, ok := in["value"]
	if !ok || value == nil {
		httpError(w, 400, "Missing proxy URL value")
		return
	}
	values, e := s.proxySettings(r.Context())
	if e != nil {
		httpError(w, 503, "Proxy configuration unavailable")
		return
	}
	mode := values["proxyMode"]
	if mode == "" {
		mode = "none"
	}
	if mode == "none" && boolean(values["proxyEnabled"]) {
		mode = "http_socks"
	}
	if mode != "none" && mode != "http_socks" {
		httpError(w, 422, "Selected gateway configuration must use the dedicated proxy settings flow")
		return
	}
	raw := str(value)
	if raw == compatSecretMask {
		if values["proxyUrl"] == "" {
			httpError(w, 400, "No saved proxy secret exists to preserve")
			return
		}
		w.WriteHeader(204)
		return
	}
	if e = proxyroute.Validate(proxyroute.Config{ProxyURL: raw}); e != nil || mode == "http_socks" && raw == "" {
		httpError(w, 422, "Invalid HTTP/SOCKS proxy URL")
		return
	}
	route := proxyroute.Config{}
	if mode == "http_socks" {
		route.ProxyURL = raw
	}
	for _, name := range s.Providers.Names() {
		if _, e := s.Providers.ConfigSchema(name); e == nil {
			if e = s.Providers.ValidateConfigurationWithRouting(name, map[string]string{}, route); e != nil {
				httpError(w, 422, "Unsupported provider route")
				return
			}
		}
	}
	values["proxyMode"] = mode
	values["proxyEnabled"] = strconv.FormatBool(mode != "none")
	values["proxyUrl"] = raw
	values["proxySslVerify"] = "true"
	values[accelerateApprovalKey] = ""
	if e = s.commitProxyChange(r.Context(), values, nil); e != nil {
		httpError(w, 500, e.Error())
		return
	}
	w.WriteHeader(204)
}
