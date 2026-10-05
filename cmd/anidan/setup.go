// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AniBakaBaka/AniDan/internal/config"
	"github.com/AniBakaBaka/AniDan/internal/server"
	"github.com/go-sql-driver/mysql"
)

//go:embed setup.html
var setupHTML string

type installationInput struct {
	Token         string `json:"token"`
	Driver        string `json:"driver"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	Password      string `json:"password"`
	Database      string `json:"database"`
	TLS           string `json:"tls"`
	Legacy        bool   `json:"legacy"`
	LegacyConfig  string `json:"legacyConfig"`
	LegacyCompose string `json:"legacyCompose"`
	LegacyHost    string `json:"legacyHost"`
	DataDir       string `json:"dataDir"`
	JWTSecret     string `json:"jwtSecret"`
	JWTAlgorithm  string `json:"jwtAlgorithm"`
	Timezone      string `json:"timezone"`
	AdminUsername string `json:"adminUsername"`
	AdminPassword string `json:"adminPassword"`
}

func installationConfig(base config.Config, in installationInput) (config.Config, error) {
	c := base
	loadedLegacy := false
	if in.Legacy {
		if in.DataDir == "" {
			in.DataDir = "/app/config"
		}
		legacy, loaded, err := installationLegacy(in)
		if err != nil {
			return c, err
		}
		loadedLegacy = loaded
		// Only import settings belonging to the original database and accounts.
		// AniDan's listener, cache, file permissions and runtime limits stay local.
		c.JWTSecret, c.JWTAlgorithm, c.Timezone = legacy.JWTSecret, legacy.JWTAlgorithm, legacy.Timezone
		if loaded {
			c.Driver, c.DSN = legacy.Driver, legacy.DSN
		}
	}
	c.LegacyDatabase = in.Legacy
	if in.DataDir != "" {
		c.DataDir = in.DataDir
	}
	var err error
	c.DataDir, err = filepath.Abs(c.DataDir)
	if err != nil {
		return c, err
	}
	if in.Timezone != "" {
		c.Timezone = in.Timezone
	}
	if in.JWTSecret != "" {
		c.JWTSecret = in.JWTSecret
	}
	if in.JWTAlgorithm != "" {
		c.JWTAlgorithm = in.JWTAlgorithm
	}
	c.AdminUsername = strings.TrimSpace(in.AdminUsername)
	if c.AdminUsername == "" {
		c.AdminUsername = "admin"
	}
	c.AdminPassword = in.AdminPassword
	// Leaving host blank with a legacy YAML selects its original SQL connection.
	if !in.Legacy || !loadedLegacy || in.Host != "" {
		c.Driver = in.Driver
		if c.Driver == "sqlite" {
			c.DSN = filepath.Join(c.DataDir, "anidan.db")
		} else {
			if strings.TrimSpace(in.Host) == "" || in.User == "" || in.Database == "" {
				return c, errors.New("请填写数据库主机、用户名和数据库名")
			}
			if in.Port == 0 {
				if c.Driver == "mysql" {
					in.Port = 3306
				} else {
					in.Port = 5432
				}
			}
			if in.Port < 1 || in.Port > 65535 {
				return c, errors.New("数据库端口无效")
			}
			switch c.Driver {
			case "mysql":
				v := mysql.NewConfig()
				v.User = in.User
				v.Passwd = in.Password
				v.Net = "tcp"
				v.Addr = net.JoinHostPort(in.Host, strconv.Itoa(in.Port))
				v.DBName = in.Database
				v.ParseTime = true
				v.Params = map[string]string{"charset": "utf8mb4"}
				if in.TLS == "require" {
					v.TLSConfig = "true"
				} else if in.TLS != "disable" {
					return c, errors.New("MySQL TLS 选项无效")
				}
				c.DSN = v.FormatDSN()
			case "postgres":
				if in.TLS != "disable" && in.TLS != "require" && in.TLS != "verify-full" {
					return c, errors.New("PostgreSQL TLS 选项无效")
				}
				u := url.URL{Scheme: "postgres", Host: net.JoinHostPort(in.Host, strconv.Itoa(in.Port)), Path: "/" + in.Database, User: url.UserPassword(in.User, in.Password)}
				q := u.Query()
				q.Set("sslmode", in.TLS)
				u.RawQuery = q.Encode()
				c.DSN = u.String()
			default:
				return c, errors.New("请选择 SQLite、MySQL 或 PostgreSQL")
			}
		}
	}
	if c.Driver == "postgresql" || c.Driver == "pgx" {
		c.Driver = "postgres"
	}
	if in.Legacy && in.LegacyHost != "" {
		if in.Host != "" {
			return c, errors.New("仅修改主机与完整数据库配置不能同时填写")
		}
		if err := installationLegacyHost(&c, in.LegacyHost); err != nil {
			return c, err
		}
	}
	if !in.Legacy && c.JWTSecret == "" {
		c.JWTSecret = rand.Text() + rand.Text()
	}
	if len(c.JWTSecret) < 16 {
		return c, errors.New("JWT 密钥至少需要 16 个字符；旧库必须使用原版实际密钥")
	}
	return c, c.Validate()
}

func runSetup(ctx context.Context, base config.Config) (config.Config, error) {
	var result config.Config
	if err := os.MkdirAll(base.DataDir, 0700); err != nil {
		return result, err
	}
	token := rand.Text()
	fmt.Fprintf(os.Stderr, "AniDan 首次安装：打开服务地址，填写一次性安装码 %s\n", token)
	var mu sync.Mutex
	done := make(chan config.Config, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"setup"}`)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, setupHTML)
	})
	mux.HandleFunc("POST /api/setup", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "需要 JSON 请求", 415)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host {
				http.Error(w, "来源不匹配", 403)
				return
			}
		}
		var in installationInput
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
		d.DisallowUnknownFields()
		if err := d.Decode(&in); err != nil {
			http.Error(w, "配置格式无效", 400)
			return
		}
		var extra any
		if d.Decode(&extra) != io.EOF {
			http.Error(w, "配置格式无效", 400)
			return
		}
		if subtle.ConstantTimeCompare([]byte(in.Token), []byte(token)) != 1 {
			http.Error(w, "安装码错误，请查看容器日志", 403)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		path := filepath.Join(base.DataDir, "anidan.json")
		if _, err := os.Stat(path); err == nil {
			http.Error(w, "已完成安装，请刷新页面", 409)
			return
		} else if !os.IsNotExist(err) {
			http.Error(w, "无法读取安装状态", 500)
			return
		}
		cfg, err := installationConfig(base, in)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		// Reserve and write the configuration before database work, so permission
		// and disk failures cannot leave an initialized DB without its settings.
		f, err := os.CreateTemp(base.DataDir, ".setup-config-")
		if err != nil {
			http.Error(w, "无法保存安装配置", 500)
			return
		}
		defer os.Remove(f.Name())
		persisted := cfg
		persisted.AdminPassword = ""
		err = json.NewEncoder(f).Encode(persisted)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			http.Error(w, "无法保存安装配置", 500)
			return
		}
		check, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		warnings, err := server.PrepareInstallation(check, cfg)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err = os.Rename(f.Name(), path); err != nil {
			http.Error(w, "无法提交配置，请检查数据目录权限", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		for _, warning := range warnings {
			fmt.Fprintln(os.Stderr, "AniDan 接入提示："+warning)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "warnings": warnings})
		done <- persisted
	})
	srv := &http.Server{Addr: base.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10}
	errorsCh := make(chan error, 1)
	go func() { errorsCh <- srv.ListenAndServe() }()
	var err error
	select {
	case result = <-done:
	case err = <-errorsCh:
	case <-ctx.Done():
		err = ctx.Err()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if closeErr := srv.Shutdown(shutdown); closeErr != nil {
		srv.Close()
		if err == nil {
			err = closeErr
		}
	}
	return result, err
}
