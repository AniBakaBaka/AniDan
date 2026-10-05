// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/AniBakaBaka/AniDan/internal/config"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Keep the cause for classification, never format raw driver errors: they may
// contain passwords, DSNs or server-provided account and database names.
type migrationPreflightError struct {
	stage string
	cause error
}

func (e *migrationPreflightError) Error() string {
	return "migration target preflight failed at " + e.stage
}
func (e *migrationPreflightError) Unwrap() error { return e.cause }

func installationPreflightError(cfg config.Config, err error) error {
	var failure *migrationPreflightError
	if errors.As(err, &failure) {
		switch failure.stage {
		case "connect":
			return installationDatabaseError(cfg, failure.cause)
		case "transaction":
			return fmt.Errorf("已连接数据库，但无法开启只读检查事务：%s", installationDatabaseReason(failure.cause))
		case "files":
			if errors.Is(err, os.ErrPermission) {
				return errors.New("无法读取原数据目录或迁移记录：容器用户没有读取权限；请检查目录和文件的实际权限，挂载设置为读写并不会修改文件权限")
			}
			return errors.New("无法读取或验证数据目录中的迁移记录；请检查 migration-receipt.json 与 .anidan-migration.json 是否完整配套。普通 Misaka 旧库无需这些文件，不要创建空文件或删除已有迁移记录来跳过检查")
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errors.New("数据库安装检查已取消或超时，请检查数据库服务状态后重试")
	}
	return errors.New("数据库迁移身份校验未通过：无法读取数据库中的迁移标记，或标记与本地迁移记录不匹配；请检查数据库读取权限及所选数据库对应的数据目录")
}

func installationDatabaseError(cfg config.Config, err error) error {
	address, loopback := installationDatabaseAddress(cfg)
	message := "无法连接数据库"
	if address != "" {
		message += "（" + address + "）"
	}
	message += "：" + installationDatabaseReason(err)
	var network net.Error
	if errors.As(err, &network) || errors.Is(err, context.DeadlineExceeded) {
		message += "。数据库在另一容器时，请将两个容器加入同一 Docker 网络，并在高级选项的“仅修改数据库主机”填写数据库容器名或网络别名；挂载 config 不会连接容器网络"
		if loopback {
			message += "。当前地址是回环地址，在默认容器网络中指向 AniDan 自身"
		}
	}
	return errors.New(message)
}

func installationDatabaseReason(err error) string {
	var my *mysql.MySQLError
	if errors.As(err, &my) {
		switch my.Number {
		case 1045:
			return "账号认证失败，请检查原数据库用户名、密码及该账号允许连接的来源"
		case 1044, 1142:
			return "数据库账号权限不足，请检查对原数据库的访问权限"
		case 1049:
			return "数据库不存在，请检查读取到的原数据库名"
		case 1130:
			return "数据库拒绝当前容器来源，请检查数据库账号的来源授权"
		default:
			return fmt.Sprintf("MySQL 返回错误码 %d，请检查数据库服务与连接设置", my.Number)
		}
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch {
		case strings.HasPrefix(pg.Code, "28"):
			return "账号认证或来源授权失败，请检查用户名、密码及 pg_hba.conf"
		case pg.Code == "3D000":
			return "数据库不存在，请检查读取到的原数据库名"
		case pg.Code == "42501":
			return "数据库账号权限不足，请检查对原数据库的访问权限"
		default:
			return "PostgreSQL 拒绝请求，请检查数据库服务与连接设置"
		}
	}
	var certificate *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	if errors.As(err, &certificate) || errors.As(err, &authority) || errors.As(err, &hostname) {
		return "TLS 证书验证失败，请检查数据库证书与连接主机名"
	}
	if errors.Is(err, context.Canceled) {
		return "连接检查已取消"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "数据库主机名无法解析，请检查容器名称和网络别名"
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout() {
		return "数据库连接超时，请检查地址、端口、防火墙及数据库运行状态"
	}
	if errors.As(err, &network) {
		return "数据库网络连接失败，请检查地址、端口、监听设置及数据库运行状态"
	}
	return "连接配置或协议检查失败，请检查主机、端口、账号、数据库名及 TLS 设置"
}

// Show only a parsed host and port. Never expose a raw DSN or parser error.
func installationDatabaseAddress(cfg config.Config) (string, bool) {
	var host, port string
	switch cfg.Driver {
	case "mysql":
		c, err := mysql.ParseDSN(cfg.DSN)
		if err != nil || c.Net != "tcp" {
			return "", false
		}
		host, port, err = net.SplitHostPort(c.Addr)
		if err != nil {
			return "", false
		}
	case "postgres", "postgresql", "pgx":
		c, err := pgx.ParseConfig(cfg.DSN)
		if err != nil {
			return "", false
		}
		host, port = c.Host, strconv.Itoa(int(c.Port))
	default:
		return "", false
	}
	ip := net.ParseIP(host)
	if host == "" || len(host) > 253 || ip == nil && strings.IndexFunc(host, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_')
	}) >= 0 {
		return "", false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", false
	}
	return net.JoinHostPort(host, port), ip.IsLoopback() || strings.EqualFold(host, "localhost")
}
