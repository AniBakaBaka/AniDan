// SPDX-License-Identifier: AGPL-3.0-only
// SC3 flat/nested message compatibility follows pinned Misaka notification
// adapters; no upstream Python or bot is executed by this implementation.
package notify

import (
	"context"
	"errors"
	"math"
	"net/url"
	"sort"
	"strconv"
	"time"
)

const MaxPollUpdates = 100

// Poll performs one bounded long poll. It never registers or removes webhooks.
// Returned authorized updates are in ascending update_id order. next also
// acknowledges unsupported/unauthorized events, avoiding a poisoned cursor.
// A transport, envelope or ID error returns the original offset and no updates.
// Callers must checkpoint only after admitting each update to their durable
// queue, then persist next after all returned updates were admitted.
func (s *Service) Poll(ctx context.Context, c Channel, offset int64) ([]Update, int64, error) {
	if err := Validate(c); err != nil {
		return nil, offset, err
	}
	if !c.Enabled {
		return nil, offset, ErrForbidden
	}
	if (c.Type != "telegram" && c.Type != "serverchan3") || Str(c.Config["mode"]) != "polling" {
		return nil, offset, ErrUnsupported
	}
	if offset < 0 {
		return nil, offset, errors.New("poll offset must be nonnegative")
	}
	wait := num(c.Config["polling_timeout"])
	if wait == 0 {
		wait = 10
	}
	q := url.Values{"offset": {strconv.FormatInt(offset, 10)}, "limit": {strconv.Itoa(MaxPollUpdates)}, "timeout": {strconv.FormatInt(wait, 10)}}
	if c.Type == "telegram" {
		q.Set("allowed_updates", `["message","callback_query"]`)
	}
	client := *s.Client
	client.Timeout = time.Duration(wait+5) * time.Second
	v, err := s.requestBody(ctx, c, "GET", s.botURL(c, "getUpdates")+"?"+q.Encode(), nil, "application/json", &client, s.pollGate)
	if err != nil {
		return nil, offset, err
	}
	if err = botOK(v); err != nil {
		return nil, offset, err
	}
	items, ok := v["result"].([]any)
	if !ok {
		return nil, offset, errors.New("bot polling response has no update list")
	}
	if len(items) > MaxPollUpdates {
		return nil, offset, ErrLimit
	}
	type item struct {
		id   int64
		data map[string]any
	}
	batch := make([]item, 0, len(items))
	for _, raw := range items {
		data := obj(raw)
		id, err := strconv.ParseInt(Str(data["update_id"]), 10, 64)
		if err != nil || id < 0 || id == math.MaxInt64 {
			return nil, offset, errors.New("bot polling response contains invalid update_id")
		}
		batch = append(batch, item{id, data})
	}
	sort.SliceStable(batch, func(i, j int) bool { return batch[i].id < batch[j].id })
	updates := make([]Update, 0, len(batch))
	next := offset
	for _, item := range batch {
		if item.id < next {
			continue // Old or repeated IDs cannot dispatch commands again.
		}
		u, err := parseBotUpdate(c, item.data)
		if err != nil && !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrUnsupported) {
			return nil, offset, err
		}
		next = item.id + 1
		if u != nil {
			updates = append(updates, *u)
		}
	}
	return updates, next, nil
}
