// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/config"
	"github.com/AniBakaBaka/AniDan/internal/store"
)

// PrepareInstallation validates the selected database before creating credentials.
// It does not launch workers, recover tasks, or start scheduled jobs.
func PrepareInstallation(ctx context.Context, cfg config.Config) (warnings []string, err error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.LegacyDatabase && cfg.Driver != "mysql" && cfg.Driver != "postgres" {
		return nil, errors.New("旧库接入仅支持 MySQL 或 PostgreSQL")
	}
	if !cfg.LegacyDatabase && (utf8.RuneCountInString(cfg.AdminPassword) < 8 || len(cfg.AdminPassword) > 72) {
		return nil, errors.New("管理员密码须至少 8 个字符，且不超过 72 个 UTF-8 字节")
	}
	if cfg.LegacyDatabase && len(cfg.JWTSecret) < 16 {
		return nil, errors.New("请填写原版实际使用的 JWT 密钥，或提供包含密钥的 config.yml")
	}
	if cfg.LegacyDatabase {
		info, err := os.Stat(cfg.DataDir)
		if err != nil || !info.IsDir() {
			return nil, errors.New("原版 config 目录不存在，请先挂载完整目录")
		}
	}
	if err := PreflightMigrationTarget(ctx, cfg); err != nil {
		return nil, installationPreflightError(cfg, err)
	}
	if !cfg.LegacyDatabase {
		if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
			return nil, errors.New("无法创建数据目录")
		}
	}
	s, err := store.Open(ctx, cfg.Driver, cfg.DSN)
	if err != nil {
		return nil, installationDatabaseError(cfg, err)
	}
	defer s.Close()
	if cfg.LegacyDatabase {
		if err := s.VerifySchema(ctx); err != nil {
			return nil, errors.New("旧库表结构不兼容当前版本，未修改表结构：" + err.Error())
		}
		count, err := s.Count(ctx, "users", nil)
		if err != nil || count == 0 {
			return nil, errors.New("旧库中没有可登录的用户")
		}
	} else {
		tables, err := s.DatabaseTables(ctx)
		if err != nil {
			return nil, errors.New("无法读取数据库表")
		}
		if len(tables) > 0 {
			return nil, errors.New("新建模式需要空数据库；已有 Misaka 数据请选择接入旧库")
		}
	}
	if err := s.SetTimezone(cfg.Timezone); err != nil {
		return nil, err
	}
	if cfg.LegacyDatabase {
		temporary := &Server{Store: s, DataDir: cfg.DataDir, Config: cfg}
		warnings, err = temporary.installationFileWarnings(ctx)
		if err != nil {
			return nil, err
		}
	}
	// Check the actual runtime UID can persist files before modifying a new DB.
	f, err := os.CreateTemp(cfg.DataDir, ".installation-")
	if err != nil {
		return nil, installationDirectoryError(cfg.DataDir, err)
	}
	f.Close()
	os.Remove(f.Name())
	for _, dir := range []string{"danmaku", "image", "logs"} {
		path := filepath.Join(cfg.DataDir, dir)
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, installationDirectoryError(path, err)
		}
		probe, err := os.CreateTemp(path, ".installation-")
		if err != nil {
			return nil, installationDirectoryError(path, err)
		}
		probe.Close()
		os.Remove(probe.Name())
	}
	if err := InitializeStore(ctx, cfg, s); err != nil {
		return nil, errors.New("数据库初始化失败，请检查表结构和数据库权限")
	}
	if err := (&Server{Store: s, DataDir: cfg.DataDir, Config: cfg}).initializeAuth(ctx); err != nil {
		return nil, err
	}
	return warnings, nil
}
