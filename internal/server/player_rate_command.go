// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"fmt"
	"strings"
)

// playerRateCommand reports native in-process protection, never upstream paid
// quotas or the migrated counters as active policy. Output stays bounded even
// when the limiter has many provider buckets.
func (s *Server) playerRateCommand(ctx context.Context) string {
	parts := []string{}
	if s.RequestLimits == nil {
		parts = append(parts, "本地请求限流器未初始化")
	} else {
		status := s.RequestLimits.Status()
		state := "启用"
		if !status.Enabled {
			state = "速率限制关闭（并发与源站退避仍生效）"
		}
		if status.Closed {
			state = "已关闭"
		}
		global := status.Global
		parts = append(parts, fmt.Sprintf("本地保护: %s；速率%g次/秒（0=不限速），突发%d，并发%d/%d；启动以来请求%d、等待%d", state, global.RequestsPerSecond, global.Burst, global.Active, global.Concurrency, global.Requests, global.Waits))
		for i, item := range status.Providers {
			if i == 20 {
				parts = append(parts, fmt.Sprintf("其余%d个源请在管理界面查看", len(status.Providers)-20))
				break
			}
			parts = append(parts, fmt.Sprintf("%s: 启动以来请求%d、等待%d，速率%g/秒，突发%d，并发%d/%d，退避%.1f秒", item.Name, item.Requests, item.Waits, item.RequestsPerSecond, item.Burst, item.Active, item.Concurrency, item.RetryAfterSeconds))
		}
		if len(status.Providers) == 0 {
			parts = append(parts, "本次启动尚无源请求")
		}
	}
	if s.Store != nil {
		rows, err := s.Store.Count(ctx, "rate_limit_state", nil)
		if err != nil {
			parts = append(parts, "旧SQL历史计数不可用（不影响上述本地状态）")
		} else {
			parts = append(parts, fmt.Sprintf("旧SQL历史计数: %d条记录，仅供诊断，不参与当前限流", rows))
		}
	}
	parts = append(parts, "以上不是源站授权额度；源站限制仍有效")
	return strings.Join(parts, " | ")
}
