// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"github.com/AniBakaBaka/AniDan/internal/notify"
	"log/slog"
	"time"
)

// queueOperationNotice is an explicit manual notice helper, not an automatic
// lifecycle producer. Automatic outcomes must use committed job observations,
// never handler defers or a parent that merely admitted a child.
func (s *Server) queueOperationNotice(kind, title, text string, imageURL ...string) {
	if s.Notify == nil || s.Jobs == nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
	defer cancel()
	event := notify.Event{Type: kind, Title: title, Text: text}
	if len(imageURL) > 0 && len(imageURL[0]) <= 2048 {
		event.ImageURL = imageURL[0]
	}
	if e := s.emitNotification(ctx, event); e != nil {
		slog.Warn("notification enqueue failed", "event", kind, "error", e)
	}
}
