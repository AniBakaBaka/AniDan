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
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/notify"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

var notificationAggregationKeys = []string{"notificationSurgeAggregationEnabled", "notificationSurgeWindowSeconds", "notificationSurgeThreshold"}

type notificationEventRuntime struct {
	collector          *notify.Aggregator
	configMu           sync.Mutex
	drainMu            sync.Mutex
	configurationError string
	enqueueFailures    atomic.Uint64
	logicalDuplicates  atomic.Uint64
	started            atomic.Bool
	closed             atomic.Bool
	done               chan struct{}
}

func (s *Server) initNotificationEvents(m *http.ServeMux) {
	a, _ := notify.NewAggregator(notify.DefaultAggregationConfig())
	s.notificationEvents = &notificationEventRuntime{collector: a, done: make(chan struct{})}
	_ = s.reloadNotificationAggregation(s.ctx)
	m.HandleFunc("GET /api/ui/notification/aggregation", s.operator(s.notificationAggregationGet))
	m.HandleFunc("PUT /api/ui/notification/aggregation", s.operator(s.notificationAggregationPut))
	m.HandleFunc("GET /api/ui/notification/lifecycle/status", s.operator(s.notificationLifecycleStatus))
	go s.notificationEventLoop()
}
func notificationAggregationSetting(key, value string) error {
	for _, known := range notificationAggregationKeys {
		if key == known && len(value) > 128 {
			return errors.New("notification aggregation setting is oversized")
		}
	}
	switch key {
	case "notificationSurgeAggregationEnabled":
		if value != "true" && value != "false" {
			return errors.New("notification aggregation enabled must be true or false")
		}
	case "notificationSurgeWindowSeconds":
		v, e := strconv.ParseFloat(value, 64)
		if e != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 1 || v > 3600 {
			return errors.New("notification aggregation window must be finite 1..3600 seconds")
		}
	case "notificationSurgeThreshold":
		v, e := strconv.Atoi(value)
		if e != nil || v < 1 || v > 1000 {
			return errors.New("notification aggregation threshold must be integer 1..1000")
		}
	}
	return nil
}
func (s *Server) reloadNotificationAggregation(ctx context.Context) error {
	n := s.notificationEvents
	if n == nil {
		return nil
	}
	n.configMu.Lock()
	defer n.configMu.Unlock()
	values := map[string]string{notificationAggregationKeys[0]: "true", notificationAggregationKeys[1]: "30", notificationAggregationKeys[2]: "5"}
	rows, err := s.Store.DB.QueryContext(ctx, s.Store.Rebind("SELECT config_key,SUBSTR(config_value,1,129) FROM config WHERE config_key IN (?,?,?)"), notificationAggregationKeys[0], notificationAggregationKeys[1], notificationAggregationKeys[2])
	if err != nil {
		n.configurationError = "Unable to read notification aggregation configuration"
		return errors.New(n.configurationError)
	}
	for rows.Next() {
		var k, v string
		if err = rows.Scan(&k, &v); err != nil {
			break
		}
		values[k] = v
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		n.configurationError = "Unable to read notification aggregation configuration"
		return errors.New(n.configurationError)
	}
	for _, k := range notificationAggregationKeys {
		if err = notificationAggregationSetting(k, values[k]); err != nil {
			n.configurationError = err.Error()
			return err
		}
	}
	seconds, _ := strconv.ParseFloat(values[notificationAggregationKeys[1]], 64)
	threshold, _ := strconv.Atoi(values[notificationAggregationKeys[2]])
	err = n.collector.Configure(notify.AggregationConfig{Enabled: values[notificationAggregationKeys[0]] == "true", Window: time.Duration(seconds * float64(time.Second)), Threshold: threshold})
	if err == nil {
		n.configurationError = ""
	}
	return err
}
func (s *Server) notificationAggregationGet(w http.ResponseWriter, r *http.Request) {
	n := s.notificationEvents
	if n == nil {
		httpError(w, 503, "Notification aggregation is unavailable")
		return
	}
	c := n.collector.Config()
	stats := n.collector.Stats()
	n.configMu.Lock()
	problem := n.configurationError
	n.configMu.Unlock()
	writeJSON(w, 200, map[string]any{"enabled": c.Enabled, "windowSeconds": c.Window.Seconds(), "threshold": c.Threshold, "limits": map[string]any{"maxCount": notify.AggregateMaxCount, "maxBuckets": notify.AggregateMaxBuckets, "maxBufferedBytes": notify.AggregateMaxBytes}, "stats": stats, "degraded": problem != "" || stats.Expired > 0 || stats.Rejected > 0 || n.enqueueFailures.Load() > 0, "configurationError": problem})
}
func (s *Server) notificationAggregationPut(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Enabled   *bool    `json:"enabled"`
		Window    *float64 `json:"windowSeconds"`
		Threshold *int     `json:"threshold"`
	}
	if err := readJSON(r, &p); err != nil || p.Enabled == nil || p.Window == nil || p.Threshold == nil {
		httpError(w, 422, "enabled, windowSeconds and threshold are required")
		return
	}
	if math.IsNaN(*p.Window) || math.IsInf(*p.Window, 0) || *p.Window < 1 || *p.Window > 3600 || *p.Threshold < 1 || *p.Threshold > 1000 {
		httpError(w, 422, "windowSeconds must be finite 1..3600 and threshold integer 1..1000")
		return
	}
	values := map[string]string{notificationAggregationKeys[0]: strconv.FormatBool(*p.Enabled), notificationAggregationKeys[1]: strconv.FormatFloat(*p.Window, 'f', -1, 64), notificationAggregationKeys[2]: strconv.Itoa(*p.Threshold)}
	if err := s.setSettingsWithHistory(r.Context(), values, "notification-ui"); err != nil {
		httpError(w, 500, "Unable to persist notification aggregation settings")
		return
	}
	if err := s.reloadNotificationAggregation(r.Context()); err != nil {
		httpError(w, 503, "Settings were saved but runtime reload is unconfirmed; reread before retrying")
		return
	}
	s.notificationAggregationGet(w, r)
}
func (s *Server) notificationLifecycleStatus(w http.ResponseWriter, r *http.Request) {
	if s.notificationEvents == nil || s.Jobs == nil {
		httpError(w, 503, "Notification lifecycle is unavailable")
		return
	}
	n := s.notificationEvents
	o := s.Jobs.LifecycleStats()
	a := n.collector.Stats()
	n.configMu.Lock()
	problem := n.configurationError
	n.configMu.Unlock()
	od := o.Dropped > 0 || o.Panics > 0 || o.Timeouts > 0
	ad := a.Rejected > 0 || a.Expired > 0 || n.enqueueFailures.Load() > 0 || problem != ""
	raw, _ := json.Marshal(a)
	var stats map[string]any
	_ = json.Unmarshal(raw, &stats)
	stats["enqueueFailures"] = n.enqueueFailures.Load()
	stats["logicalDuplicates"] = n.logicalDuplicates.Load()
	stats["degraded"] = ad
	writeJSON(w, 200, map[string]any{"observer": map[string]any{"queued": o.Queued, "queuedBytes": o.QueuedBytes, "observedCallbacks": o.Delivered, "dropped": o.Dropped, "panics": o.Panics, "timeouts": o.Timeouts, "active": o.Active, "degraded": od}, "aggregation": stats, "degraded": od || ad, "guarantee": "Bounded observation and durable admission; remote delivery is not exactly-once"})
}
func (s *Server) notificationEventLoop() {
	n := s.notificationEvents
	defer close(n.done)
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
			_ = s.reloadNotificationAggregation(ctx)
			_ = s.drainNotificationEvents(ctx)
			cancel()
		}
	}
}
func (s *Server) closeNotificationEvents() {
	n := s.notificationEvents
	if n == nil || !n.closed.CompareAndSwap(false, true) {
		return
	}
	n.collector.Close(time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = s.drainNotificationEvents(ctx)
	cancel()
	stats := n.collector.Stats()
	if stats.Pending > 0 || stats.Buckets > 0 {
		slog.Warn("notification shutdown left unadmitted buffered events", "pendingBatches", stats.Pending, "buckets", stats.Buckets)
	}
}
func notificationTargetFingerprint(row store.Row, c notify.Channel) string {
	raw, _ := json.Marshal(struct {
		ID               int64
		Kind             string
		Config, Events   map[string]any
		Proxy            bool
		Created, Updated string
	}{c.ID, c.Type, c.Config, c.EventsConfig, c.UseProxy, str(row["created_at"]), str(row["updated_at"])})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func (s *Server) notificationTargets(ctx context.Context, kind string) ([]notify.DeliveryTarget, error) {
	rows, err := s.notificationEnabledRows(ctx)
	if err != nil {
		return nil, errors.New("Unable to read subscribed notification channels")
	}
	out := []notify.DeliveryTarget{}
	for _, row := range rows {
		c, e := notificationDecode(row)
		if e != nil {
			return nil, e
		}
		if notify.Bool(c.EventsConfig[kind]) || notify.Bool(c.EventsConfig["*"]) {
			out = append(out, notify.DeliveryTarget{ID: c.ID, Fingerprint: notificationTargetFingerprint(row, c)})
		}
	}
	return out, nil
}

// Exclusions are exact configuration-bound audience pairs. Existing rich final
// delivery for other subscribed destinations keeps its independent admission.
func notificationExcludeTargets(targets, excluded []notify.DeliveryTarget) []notify.DeliveryTarget {
	if len(excluded) == 0 {
		return targets
	}
	owned := make(map[notify.DeliveryTarget]bool, len(excluded))
	for _, t := range excluded {
		owned[t] = true
	}
	kept := targets[:0]
	for _, t := range targets {
		if !owned[t] {
			kept = append(kept, t)
		}
	}
	return kept
}
func (s *Server) collectNotificationEvent(ctx context.Context, event notify.Event, id, group string, window bool) error {
	return s.collectNotificationEventExcept(ctx, event, id, group, window, nil)
}
func (s *Server) collectNotificationEventExcept(ctx context.Context, event notify.Event, id, group string, window bool, excluded []notify.DeliveryTarget) error {
	n := s.notificationEvents
	if n == nil || n.closed.Load() {
		return notify.ErrAggregationClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	targets, err := s.notificationTargets(ctx, event.Type)
	if err != nil {
		n.enqueueFailures.Add(1)
		return err
	}
	targets = notificationExcludeTargets(targets, excluded)
	if len(targets) == 0 {
		return nil
	}
	if id == "" {
		id = randomID()
	}
	key, fresh, err := s.reserveLogicalNotification(ctx, id, event.Type)
	if err != nil {
		n.enqueueFailures.Add(1)
		return err
	}
	if !fresh {
		n.logicalDuplicates.Add(1)
		return nil
	}
	err = n.collector.Collect(time.Now(), notify.AggregateNotice{ID: id, Group: group, Event: event, Targets: targets, ForceWindow: window})
	if err != nil {
		n.enqueueFailures.Add(1)
		// This process created the exact reservation but did not accept the notice.
		// A failed cleanup remains fail-closed; never overwrite an existing receipt.
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = s.Store.DB.ExecContext(cleanup, s.Store.Rebind("DELETE FROM cache_data WHERE cache_key=? AND cache_provider='notification_events'"), key)
		cancel()
		return err
	}
	return s.drainNotificationEvents(ctx)
}
func (s *Server) drainNotificationEvents(ctx context.Context) error {
	n := s.notificationEvents
	if n == nil || s.Jobs == nil {
		return nil
	}
	if !n.drainMu.TryLock() {
		if !n.closed.Load() {
			return nil
		}
		// Shutdown waits cooperatively for an in-flight admission, but only inside
		// its shared finite drain budget. Do not cancel the manager before this.
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				if n.drainMu.TryLock() {
					goto acquired
				}
			}
		}
	}
acquired:
	defer n.drainMu.Unlock()
	for _, batch := range n.collector.Pending(time.Now(), notify.AggregateMaxBuckets) {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, _, err := s.Jobs.SubmitWithReceipt(ctx, "notification_delivery", batch, job.SubmitOptions{Title: "发送已订阅通知", QueueType: "notification"}, "notification-outbound-admission:"+batch.ID, time.Now().Add(7*24*time.Hour))
		if err != nil {
			n.enqueueFailures.Add(1)
			return err
		}
		n.collector.Ack(batch.ID)
	}
	return nil
}

// Each batch/target gets one recorded send attempt. Ambiguous transport failures
// are not retried, even when an operator retries the durable notification job.
// A crash after this reservation but before HTTP can omit a notice; it must not
// be disguised as remote exactly-once delivery. Receipts use a protected region.
func (s *Server) reserveNotificationAttempt(ctx context.Context, batchID string, targetID int64) (string, bool, error) {
	key := fmt.Sprintf("notification-outbound-attempt:%s:%d", batchID, targetID)
	_, err := s.Store.Insert(ctx, "cache_data", store.Row{"cache_key": key, "cache_value": `{"status":"attempted"}`, "cache_provider": "notification_events", "expires_at": time.Now().In(s.authLocation()).Add(7 * 24 * time.Hour).Format("2006-01-02T15:04:05.999999")})
	if err == nil {
		return key, true, nil
	}
	if _, readErr := s.Store.Get(ctx, "cache_data", key); readErr == nil {
		return key, false, nil
	}
	return "", false, errors.New("Unable to record notification send attempt")
}
func (s *Server) deliverNotificationBatch(ctx context.Context, b notify.DeliveryBatch) error {
	if len(b.ID) != 64 || b.Count < 1 || b.Count > notify.AggregateMaxCount || len(b.Targets) < 1 || len(b.Targets) > notify.AggregateMaxTargets || b.CreatedAt.IsZero() || b.CreatedAt.After(time.Now().Add(5*time.Minute)) || time.Since(b.CreatedAt) > 7*24*time.Hour {
		return errors.New("invalid or expired notification batch")
	}
	if _, err := hex.DecodeString(b.ID); err != nil {
		return errors.New("invalid notification batch identity")
	}
	var errs []error
	attempted := 0
	accepted := 0
	var picture []byte
	imagePrepared := false
	for _, target := range b.Targets {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		row, err := s.Store.Get(ctx, "notification_channels", target.ID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return errors.New("Unable to read notification target")
		}
		c, err := notificationDecode(row)
		if err != nil {
			return err
		}
		if !c.Enabled || (!notify.Bool(c.EventsConfig[b.Event.Type]) && !notify.Bool(c.EventsConfig["*"])) || notificationTargetFingerprint(row, c) != target.Fingerprint {
			continue
		}
		m := notify.Message{Title: b.Event.Title, Text: b.Event.Text}
		if err = notify.ValidateMessageShape(c, m); err != nil {
			errs = append(errs, errors.New("Notification target failed message preflight"))
			continue
		}
		if b.Event.ImageURL != "" && notificationImageMode(c) != "text" && c.Type != "serverchan3" {
			if !imagePrepared {
				imagePrepared = true
				picture, _ = s.notificationPreparePoster(ctx, b.Event.ImageURL, libPosterClient())
			}
			m.Image = picture
			m.ImageShareable = len(picture) > 0
		}
		key, fresh, e := s.reserveNotificationAttempt(ctx, b.ID, target.ID)
		if e != nil {
			return e
		}
		if !fresh {
			errs = append(errs, errors.New("Notification attempt already recorded; inspect recipient before issuing a new notice"))
			continue
		}
		attempted++
		err = s.notificationSend(ctx, c, m)
		state := "accepted"
		if err != nil {
			state = "uncertain"
			errs = append(errs, fmt.Errorf("Notification target %d: %w", target.ID, err))
		} else {
			accepted++
		}
		if e = s.Store.Update(ctx, "cache_data", key, store.Row{"cache_value": fmt.Sprintf(`{"status":%q}`, state)}); e != nil {
			errs = append(errs, errors.New("Notification attempt outcome could not be recorded"))
		}
	}
	if len(errs) > 0 {
		if accepted > 0 {
			return errors.Join(notify.ErrPartialDelivery, errors.Join(errs...))
		}
		if attempted > 0 {
			return errors.Join(notify.ErrDeliveryUncertain, errors.Join(errs...))
		}
		return errors.Join(errs...)
	}
	return nil
}

func (s *Server) reserveLogicalNotification(ctx context.Context, id, kind string) (string, bool, error) {
	if len(id) > 512 || id == "" || len(kind) > 80 {
		return "", false, notify.ErrLimit
	}
	sum := sha256.Sum256([]byte(kind + "\x00" + id))
	key := "notification-logical-event:" + hex.EncodeToString(sum[:])
	_, err := s.Store.Insert(ctx, "cache_data", store.Row{"cache_key": key, "cache_value": `{"status":"accepted"}`, "cache_provider": "notification_events", "expires_at": time.Now().In(s.authLocation()).Add(7 * 24 * time.Hour).Format("2006-01-02T15:04:05.999999")})
	if err == nil {
		return key, true, nil
	}
	if _, e := s.Store.Get(ctx, "cache_data", key); e == nil {
		return key, false, nil
	}
	return "", false, errors.New("Unable to reserve logical notification identity")
}

// Bound channel configuration before it reaches Go memory. Store.List's generic
// row-count cap alone is insufficient for imported opaque configuration blobs.
func (s *Server) notificationEnabledRows(ctx context.Context) ([]store.Row, error) {
	rows, err := s.Store.DB.QueryContext(ctx, s.Store.Rebind("SELECT id,SUBSTR(name,1,501) AS name,channel_type,is_enabled,use_proxy,SUBSTR(config,1,65537) AS config,SUBSTR(events_config,1,16385) AS events_config,created_at,updated_at FROM notification_channels WHERE is_enabled=? ORDER BY id LIMIT 1001"), true)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []store.Row{}
	total := 0
	for rows.Next() {
		row, e := store.ScanRow(rows, store.Schema["notification_channels"])
		if e != nil {
			return nil, e
		}
		config, events := str(row["config"]), str(row["events_config"])
		total += len(config) + len(events) + 2048
		if len(config) > 65536 || len(events) > 16384 || total > 4<<20 || len(out) >= 1000 {
			return nil, errors.New("Enabled notification configuration exceeds bounded runtime limits")
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
