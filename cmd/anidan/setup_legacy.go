// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/config"
	"github.com/AniBakaBaka/AniDan/internal/migrate"
	"github.com/go-sql-driver/mysql"
)

func installationLegacy(in installationInput) (config.Config, bool, error) {
	empty := config.Defaults()
	configPath := in.LegacyConfig
	if configPath == "" {
		configPath = filepath.Join(in.DataDir, "config.yml")
	}
	raw, err := installationReadSettings(configPath)
	if err != nil && !(in.LegacyConfig == "" && os.IsNotExist(err)) {
		return empty, false, errors.New("无法读取原版 config.yml，请检查挂载路径、文件权限与大小（最多 1 MiB）")
	}
	composePath := in.LegacyCompose
	// Only inspect the selected config directory, never unrelated parent folders.
	// An explicit config path opts out of automatic Compose discovery.
	if composePath == "" && in.LegacyConfig == "" {
		for _, name := range []string{"compose.yml", "compose.yaml", "docker-compose.yml", "docker-compose.yaml", "compose.override.yml", "compose.override.yaml", "docker-compose.override.yml", "docker-compose.override.yaml"} {
			path := filepath.Join(in.DataDir, name)
			if _, e := os.Lstat(path); os.IsNotExist(e) {
				continue
			}
			if composePath != "" || strings.Contains(name, ".override.") {
				return empty, false, errors.New("挂载目录包含多份 Compose 配置，请在高级选项指定实际使用的配置文件")
			}
			composePath = path
		}
	}
	if composePath != "" {
		compose, e := installationReadSettings(composePath)
		if e != nil {
			return empty, false, errors.New("无法读取原版 Compose，请检查文件挂载与权限（最多 1 MiB）")
		}
		installation, e := migrate.ParseLegacyInstallation(raw, compose)
		if e != nil {
			return empty, false, errors.New("无法解析原版 Compose 的实际设置；含 ${变量}、env_file 或自定义服务结构时，请在高级选项指定有效 config.yml 并填写覆盖值")
		}
		raw = installation.EffectiveYAML
	}
	if raw == nil {
		if in.Host != "" {
			return empty, false, nil // Manual installation remains available.
		}
		return empty, false, errors.New("未找到原版配置：请将完整 config 目录挂载到 /app/config；连接参数在 Compose 中时，再将原 Compose 文件只读挂载到 /app/config/docker-compose.yml")
	}
	c, err := config.TranslateLegacyForInstallation(raw)
	if err != nil {
		return empty, false, errors.New("原版配置格式无效，请检查实际 config.yml 和 Compose 设置")
	}
	if in.Host == "" && (c.DSN == "" || c.Driver == "sqlite") {
		return empty, false, errors.New("配置中没有原数据库连接信息，请挂载原 Compose 文件，或在高级选项填写连接参数")
	}
	return c, true, nil
}

func installationReadSettings(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 1<<20 {
		return nil, errors.New("invalid settings file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("cannot read settings file")
	}
	return raw, nil
}

func installationLegacyHost(c *config.Config, host string) error {
	host = strings.TrimSpace(host)
	if host == "" || strings.ContainsAny(host, "/\\@?# \t\r\n\x00") || strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return errors.New("数据库主机应为域名或 IP，不包含端口或连接字符串")
	}
	switch c.Driver {
	case "mysql":
		v, err := mysql.ParseDSN(c.DSN)
		if err != nil {
			return errors.New("原版 MySQL 连接配置无效")
		}
		_, port, err := net.SplitHostPort(v.Addr)
		if err != nil {
			return errors.New("原版 MySQL 主机配置无效")
		}
		v.Addr = net.JoinHostPort(host, port)
		c.DSN = v.FormatDSN()
	case "postgres":
		v, err := url.Parse(c.DSN)
		if err != nil || v.Host == "" {
			return errors.New("原版 PostgreSQL 连接配置无效")
		}
		v.Host = net.JoinHostPort(host, v.Port())
		c.DSN = v.String()
	default:
		return errors.New("旧库接入仅支持 MySQL 或 PostgreSQL")
	}
	return nil
}
