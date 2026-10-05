// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"database/sql"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/notify"
)

// initNotificationRouting installs a runtime-only settings reader. One SELECT
// snapshots the four settings together, avoiding mixed old/new proxy credentials
// when the operator atomically updates configuration. Errors fail closed.
// Per-operation snapshots and transport/token rotation belong to the service;
// no proxy credential or relay key enters public/stored channel configuration.
func (s *Server) initNotificationRouting() {
	s.Notify.Routing = func(ctx context.Context, _ notify.Channel) (notify.RoutingConfig, error) {
		values := map[string]string{"proxyMode": "none", "webhookApiKey": s.Config.WebhookAPIKey}
		rows, err := s.Store.DB.QueryContext(ctx, s.Store.Rebind("SELECT config_key, config_value FROM config WHERE config_key IN (?,?,?,?)"), "proxyMode", "proxyEnabled", "proxyUrl", "webhookApiKey")
		if err != nil {
			return notify.RoutingConfig{}, err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			var value sql.NullString
			if err = rows.Scan(&key, &value); err != nil {
				return notify.RoutingConfig{}, err
			}
			values[key] = value.String
		}
		if err = rows.Err(); err != nil {
			return notify.RoutingConfig{}, err
		}
		return notify.RoutingConfig{ProxyMode: values["proxyMode"], ProxyEnabled: strings.EqualFold(values["proxyEnabled"], "true"), ProxyURL: values["proxyUrl"], RelayKey: values["webhookApiKey"]}, nil
	}
}
