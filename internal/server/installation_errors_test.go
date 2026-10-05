// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/config"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestInstallationUnreachableDatabaseIsNotDirectoryFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	for _, driver := range []string{"mysql", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.LegacyDatabase, cfg.Driver = true, driver
			cfg.DataDir = t.TempDir()
			cfg.JWTSecret = "original-secret-for-test"
			if driver == "mysql" {
				c := mysql.NewConfig()
				c.Net, c.Addr, c.User, c.Passwd, c.DBName = "tcp", address, "private-user", "private-password", "private-db"
				c.Timeout = time.Second
				cfg.DSN = c.FormatDSN()
			} else {
				u := url.URL{Scheme: "postgres", Host: address, Path: "/private-db", User: url.UserPassword("private-user", "private-password"), RawQuery: "sslmode=disable&connect_timeout=1"}
				cfg.DSN = u.String()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := PrepareInstallation(ctx, cfg)
			if err == nil || !strings.Contains(err.Error(), "无法连接数据库") || !strings.Contains(err.Error(), address) || !strings.Contains(err.Error(), "同一 Docker 网络") || !strings.Contains(err.Error(), "回环地址") || strings.Contains(err.Error(), "迁移身份") {
				t.Fatalf("wrong failure classification: %v", err)
			}
			for _, secret := range []string{"private-user", "private-password", "private-db", cfg.JWTSecret} {
				if strings.Contains(err.Error(), secret) {
					t.Fatal("installation diagnostic disclosed private settings")
				}
			}
			entries, err := os.ReadDir(cfg.DataDir)
			if err != nil || len(entries) != 0 {
				t.Fatal("connection failure changed the source directory")
			}
		})
	}
}

func TestInstallationPreflightClassificationAndRedaction(t *testing.T) {
	const private = "private-password-and-dsn"
	for _, tc := range []struct {
		name, stage, want string
		cause             error
	}{
		{"mysql-auth", "connect", "账号认证失败", &mysql.MySQLError{Number: 1045, Message: private}},
		{"mysql-database", "connect", "数据库不存在", &mysql.MySQLError{Number: 1049, Message: private}},
		{"postgres-auth", "connect", "pg_hba.conf", &pgconn.PgError{Code: "28P01", Message: private}},
		{"postgres-permission", "connect", "权限不足", &pgconn.PgError{Code: "42501", Detail: private}},
		{"dns", "connect", "主机名无法解析", &net.DNSError{Name: private, IsNotFound: true}},
		{"unknown", "connect", "连接配置或协议", errors.New(private)},
		{"transaction", "transaction", "无法开启只读检查事务", &mysql.MySQLError{Number: 1044, Message: private}},
		{"file-permission", "files", "容器用户没有读取权限", &os.PathError{Op: "open", Path: private, Err: os.ErrPermission}},
		{"file-metadata", "files", "迁移记录", errors.New(private)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := &migrationPreflightError{stage: tc.stage, cause: tc.cause}
			err := installationPreflightError(config.Defaults(), fmt.Errorf("wrapped: %w", failure))
			if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), private) || strings.Contains(failure.Error(), private) {
				t.Fatalf("incorrect or unsafe diagnostic: %v", err)
			}
		})
	}
}

func TestInstallationStillRejectsIncompleteMigrationEvidence(t *testing.T) {
	cfg := config.Defaults()
	cfg.LegacyDatabase, cfg.Driver = true, "mysql"
	cfg.DataDir, cfg.JWTSecret = t.TempDir(), "original-secret-for-test"
	cfg.DSN = "user:password@tcp(127.0.0.1:1)/database"
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "migration-receipt.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := PrepareInstallation(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "迁移记录") || strings.Contains(err.Error(), "无法连接数据库") {
		t.Fatalf("migration evidence was bypassed: %v", err)
	}
}
