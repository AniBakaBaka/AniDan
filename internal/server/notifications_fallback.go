// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/job"
	"github.com/AniBakaBaka/AniDan/internal/notify"
	"github.com/AniBakaBaka/AniDan/internal/provider"
)

type notificationFallbackParams struct {
	NoticeID  string                  `json:"noticeId,omitempty"`
	CreatedAt time.Time               `json:"createdAt,omitempty"`
	Targets   []notify.DeliveryTarget `json:"targets,omitempty"`
	Keyword   string                  `json:"keyword"`
	Success   bool                    `json:"success"`
	Total     int                     `json:"total"`
	Results   []provider.SearchResult `json:"results,omitempty"`
}

// queueFallbackSearchNotice never embeds the caller's API token/hash, raw provider
// errors or an unbounded result array in durable task parameters.
func notificationFallbackPayload(keyword string, results []provider.SearchResult, outcome error) notificationFallbackParams {
	p := notificationFallbackParams{Keyword: notificationKeyword(keyword), Success: outcome == nil, Total: len(results)}
	if outcome == nil {
		for _, r := range results[:min(len(results), 5)] {
			imageURL := r.ImageURL
			if len(imageURL) > 2048 {
				imageURL = ""
			}
			p.Results = append(p.Results, provider.SearchResult{Provider: notificationClip(r.Provider, 50), Result: provider.Result{Title: notificationClip(r.Title, 100), ImageURL: imageURL}})
		}
	}
	return p
}
func (s *Server) enqueueFallbackNotice(ctx context.Context, id string, p notificationFallbackParams) error {
	return s.enqueueFallbackNoticeExcept(ctx, id, p, nil)
}
func (s *Server) enqueueFallbackNoticeExcept(ctx context.Context, id string, p notificationFallbackParams, excluded []notify.DeliveryTarget) error {
	if s.Notify == nil || s.Jobs == nil {
		return nil
	}
	targets, err := s.notificationTargets(ctx, "fallback_search_complete")
	if err != nil {
		return err
	}
	targets = notificationExcludeTargets(targets, excluded)
	if len(targets) == 0 {
		return nil
	}
	hash := sha256.Sum256([]byte(id))
	p.NoticeID = hex.EncodeToString(hash[:])
	p.CreatedAt = time.Now()
	p.Targets = targets
	_, _, err = s.Jobs.SubmitWithReceipt(ctx, "notification_fallback_search", p, job.SubmitOptions{Title: "发送后备搜索通知", QueueType: "notification"}, "notification-fallback-admission:"+p.NoticeID, time.Now().Add(7*24*time.Hour))
	return err
}
func (s *Server) queueFallbackSearchNotice(keyword string, results []provider.SearchResult, outcome error) {
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()
	if err := s.enqueueFallbackNotice(ctx, randomID(), notificationFallbackPayload(keyword, results, outcome)); err != nil {
		slog.Warn("notification enqueue failed", "event", "fallback_search_complete")
	}
}

func (s *Server) notificationFallbackJob(ctx context.Context, raw json.RawMessage, progress func(int, string)) (any, error) {
	var p notificationFallbackParams
	if len(raw) > 256<<10 || unmarshalExactJSON(raw, &p) != nil || len(p.Results) > 5 || len(p.Targets) > notify.AggregateMaxTargets {
		return nil, notify.ErrLimit
	}
	if p.NoticeID == "" {
		return notificationLegacyReview()
	}
	if p.NoticeID != "" && (len(p.NoticeID) != 64 || p.CreatedAt.IsZero() || p.CreatedAt.After(time.Now().Add(5*time.Minute)) || time.Since(p.CreatedAt) > 7*24*time.Hour) {
		return nil, errors.New("invalid or expired fallback notice")
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	progress(10, "Preparing fallback-search notification")
	count, e := s.notificationDeliverFallback(ctx, p, libPosterClient())
	if e != nil {
		return nil, e
	}
	progress(100, "Subscribed fallback-search notices accepted")
	return map[string]any{"delivered": count}, nil
}
func (s *Server) notificationDeliverFallback(ctx context.Context, p notificationFallbackParams, client libHTTPDoer) (int, error) {
	rows, e := s.notificationEnabledRows(ctx)
	if e != nil {
		return 0, e
	}
	channels := []notify.Channel{}
	wantImage := false
	for _, row := range rows {
		c, e := notificationDecode(row)
		if e != nil {
			return 0, e
		}
		if notify.Bool(c.EventsConfig["fallback_search_complete"]) || notify.Bool(c.EventsConfig["*"]) {
			if p.NoticeID != "" {
				allowed := false
				for _, target := range p.Targets {
					if target.ID == c.ID && target.Fingerprint == notificationTargetFingerprint(row, c) {
						allowed = true
						break
					}
				}
				if !allowed {
					continue
				}
			}
			channels = append(channels, c)
			wantImage = wantImage || (notificationImageMode(c) != "text" && (c.Type == "telegram" || c.Type == "wechat"))
		}
	}
	text := fmt.Sprintf("Fallback search completed: %s\nResults: %d", notificationClip(p.Keyword, 150), p.Total)
	if !p.Success {
		text = "Fallback search failed: " + notificationClip(p.Keyword, 150) + "\nSee the authenticated task list for details."
	}
	for i, r := range p.Results {
		text += fmt.Sprintf("\n%d. [%s] %s", i+1, notificationClip(r.Provider, 30), notificationClip(r.Title, 65))
	}
	var picture []byte
	if p.Success && wantImage && strings.EqualFold(s.setting(ctx, "fallbackSearchPosterCollage", "true"), "true") && len(p.Results) > 0 {
		select {
		case libPosterSlots <- struct{}{}:
			picture, _, e = s.notificationBuildCollage(ctx, p.Results, 0, client)
			<-libPosterSlots
			if e != nil {
				picture = nil
			} // Preparing an optional image cannot suppress the notice.
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	sent := 0
	failures := []error{}
	for _, c := range channels {
		if e = ctx.Err(); e != nil {
			return sent, e
		}
		m := notify.Message{Text: notificationClip(text, 950)}
		if c.Type == "wechat" {
			m.Text = notificationClipBytes(m.Text, 1800)
		}
		if c.Type == "telegram" || c.Type == "wechat" {
			m.Image = picture
			m.ImageShareable = true
		}
		if p.NoticeID != "" {
			_, fresh, err := s.reserveNotificationAttempt(ctx, p.NoticeID, c.ID)
			if err != nil {
				return sent, err
			}
			if !fresh {
				failures = append(failures, errors.New("Notification attempt already recorded; inspect recipient before retrying"))
				continue
			}
		}
		if e = s.notificationSend(ctx, c, m); e != nil {
			failures = append(failures, fmt.Errorf("channel %d: %s", c.ID, notificationSafeError(e)))
		} else {
			sent++
		}
	}
	return sent, errors.Join(failures...)
}

func notificationKeyword(value string) string {
	safe := notificationMediaTitle(value)
	if safe == "" {
		return "来源查询"
	}
	return safe
}
