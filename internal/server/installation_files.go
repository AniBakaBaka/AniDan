// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Direct attachment keeps the original rows in place. Stale library references
// are not installation requirements; report them without deleting or rewriting
// data. Snapshot/copy migration retains its separate completeness checks.
func (s *Server) installationFileWarnings(ctx context.Context) ([]string, error) {
	unavailable := 0
	fields := []struct {
		table   string
		columns []string
	}{
		{"episode", []string{"danmaku_file_path"}},
		{"anime", []string{"local_image_path", "image_url"}},
		{"local_danmaku_items", []string{"file_path", "nfo_path", "poster_url"}},
		{"media_items", []string{"poster_url"}},
		{"external_calendar_item", []string{"image_url"}},
	}
	for _, field := range fields {
		for offset := 0; ; offset += 500 {
			rows, err := s.Store.List(ctx, field.table, nil, 500, offset)
			if err != nil {
				return nil, errors.New("无法读取旧库文件引用，请检查数据库读取权限及连接状态")
			}
			for _, row := range rows {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				for _, column := range field.columns {
					path, _ := row[column].(string)
					if path == "" || strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "data:") || strings.HasPrefix(path, "/api/ui/media-servers/") || strings.HasPrefix(path, "/api/ui/local-items/") {
						continue
					}
					if strings.HasPrefix(path, "/data/images/") {
						path = filepath.Join(s.DataDir, "image", strings.TrimPrefix(path, "/data/images/"))
					} else if field.table == "local_danmaku_items" && column == "poster_url" && !filepath.IsAbs(path) {
						path = filepath.Join(filepath.Dir(str(row["file_path"])), path)
					}
					// Reuse the runtime's anchored, nonblocking read policy. A
					// database reference does not grant access to arbitrary files.
					file, err := s.localOpenAllowed(s.normalizeStoredPath(path))
					if err != nil {
						unavailable++
						continue
					}
					info, err := file.Stat()
					file.Close()
					if err != nil || !info.Mode().IsRegular() {
						unavailable++
					}
				}
			}
			if len(rows) < 500 {
				break
			}
		}
	}
	if unavailable == 0 {
		return nil, nil
	}
	return []string{fmt.Sprintf("旧库有 %d 项文件引用缺失、不可读或不在允许读取的目录中，已保留原记录并继续接入。这些文件暂不可用；需要的文件可在补充挂载和读取权限后恢复，不需要的历史扫描记录可登录后清理。", unavailable)}, nil
}
