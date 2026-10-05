// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/config"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

// PrepareInstallation validates the selected database before creating credentials.
// It does not launch workers, recover tasks, or start scheduled jobs.
func PrepareInstallation(ctx context.Context, cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.LegacyDatabase && cfg.Driver != "mysql" && cfg.Driver != "postgres" {
		return errors.New("旧库接入仅支持 MySQL 或 PostgreSQL")
	}
	if !cfg.LegacyDatabase && (utf8.RuneCountInString(cfg.AdminPassword) < 8 || len(cfg.AdminPassword) > 72) {
		return errors.New("管理员密码须至少 8 个字符，且不超过 72 个 UTF-8 字节")
	}
	if cfg.LegacyDatabase && len(cfg.JWTSecret) < 16 {
		return errors.New("请填写原版实际使用的 JWT 密钥，或提供包含密钥的 config.yml")
	}
	if err := PreflightMigrationTarget(ctx, cfg); err != nil {
		return errors.New("数据库迁移身份检查失败，请检查是否使用了完整的数据目录")
	}
	if cfg.LegacyDatabase {
		info, err := os.Stat(cfg.DataDir)
		if err != nil || !info.IsDir() {
			return errors.New("原版 config 目录不存在，请先挂载完整目录")
		}
	} else if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return errors.New("无法创建数据目录")
	}
	s, err := store.Open(ctx, cfg.Driver, cfg.DSN)
	if err != nil {
		return errors.New("无法连接数据库，请检查地址、数据库名、账号、密码及 TLS 设置")
	}
	defer s.Close()
	if cfg.LegacyDatabase {
		if err := s.VerifySchema(ctx); err != nil {
			return errors.New("旧库表结构不兼容当前版本，未修改表结构：" + err.Error())
		}
		count, err := s.Count(ctx, "users", nil)
		if err != nil || count == 0 {
			return errors.New("旧库中没有可登录的用户")
		}
	} else {
		tables, err := s.DatabaseTables(ctx)
		if err != nil {
			return errors.New("无法读取数据库表")
		}
		if len(tables) > 0 {
			return errors.New("新建模式需要空数据库；已有 Misaka 数据请选择接入旧库")
		}
	}
	if err := s.SetTimezone(cfg.Timezone); err != nil {
		return err
	}
	if cfg.LegacyDatabase {
		temporary := &Server{Store: s, DataDir: cfg.DataDir, Config: cfg}
		fields := map[string][]string{"episode": {"danmaku_file_path"}, "anime": {"local_image_path", "image_url"}, "local_danmaku_items": {"file_path", "nfo_path", "poster_url"}, "media_items": {"poster_url"}, "external_calendar_item": {"image_url"}}
		for table, columns := range fields {
			for offset := 0; ; offset += 500 {
				rows, err := s.List(ctx, table, nil, 500, offset)
				if err != nil {
					return errors.New("无法读取旧库文件引用")
				}
				for _, row := range rows {
					for _, column := range columns {
						path, _ := row[column].(string)
						if path == "" || strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "data:") || strings.HasPrefix(path, "/api/ui/media-servers/") || strings.HasPrefix(path, "/api/ui/local-items/") {
							continue
						}
						if strings.HasPrefix(path, "/data/images/") {
							path = filepath.Join(cfg.DataDir, "image", strings.TrimPrefix(path, "/data/images/"))
						}
						path = temporary.normalizeStoredPath(path)
						file, err := os.Open(path)
						if err != nil {
							return errors.New("旧库引用文件不可读，请检查完整 config 目录及挂载路径：" + path)
						}
						info, err := file.Stat()
						file.Close()
						if err != nil || !info.Mode().IsRegular() {
							return errors.New("旧库引用不是普通文件：" + path)
						}
					}
				}
				if len(rows) < 500 {
					break
				}
			}
		}
	}
	// Check the actual runtime UID can persist files before modifying a new DB.
	f, err := os.CreateTemp(cfg.DataDir, ".installation-")
	if err != nil {
		return errors.New("数据目录不可写，请检查容器用户权限")
	}
	f.Close()
	os.Remove(f.Name())
	for _, dir := range []string{"danmaku", "image", "logs"} {
		path := filepath.Join(cfg.DataDir, dir)
		if err := os.MkdirAll(path, 0700); err != nil {
			return errors.New("无法创建运行目录：" + path)
		}
		probe, err := os.CreateTemp(path, ".installation-")
		if err != nil {
			return errors.New("运行目录不可写：" + path)
		}
		probe.Close()
		os.Remove(probe.Name())
	}
	if err := InitializeStore(ctx, cfg, s); err != nil {
		return errors.New("数据库初始化失败，请检查表结构和数据库权限")
	}
	return (&Server{Store: s, DataDir: cfg.DataDir, Config: cfg}).initializeAuth(ctx)
}
