// SPDX-License-Identifier: AGPL-3.0-only
package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/AniBakaBaka/AniDan/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func (s *Server) initializeAuth(ctx context.Context) error {
	users, e := s.Store.Count(ctx, "users", nil)
	if e != nil {
		return e
	}
	if users == 0 && (utf8.RuneCountInString(s.Config.AdminPassword) < 8 || len([]byte(s.Config.AdminPassword)) > 72) {
		return errors.New("set ANIDAN_ADMIN_PASSWORD to 8 or more characters (at most 72 UTF-8 bytes) for first initialization")
	}
	if s.Config.JWTSecret == "" {
		keyPath := filepath.Join(s.DataDir, ".jwt-secret")
		info, e := os.Lstat(keyPath)
		if e == nil {
			if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
				return errors.New("JWT key file must be a regular owner-only file")
			}
			b, e := os.ReadFile(keyPath)
			if e != nil {
				return e
			}
			s.Config.JWTSecret = strings.TrimSpace(string(b))
		} else if errors.Is(e, os.ErrNotExist) {
			enabled, e := s.Store.List(ctx, "users", store.Row{"is_otp": true}, 10000, 0)
			if e != nil {
				return e
			}
			for _, u := range enabled {
				if !authPlainOTP(authString(u["otp_secret"])) {
					return errors.New("migrated encrypted TOTP requires the original JWT encryption key in ANIDAN_JWT_SECRET")
				}
			}
			key := authRandom()
			f, e := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if e != nil {
				return e
			}
			_, e = f.WriteString(key + "\n")
			if e == nil {
				e = f.Sync()
			}
			closeErr := f.Close()
			if e != nil {
				return e
			}
			if closeErr != nil {
				return closeErr
			}
			s.Config.JWTSecret = key
		} else {
			return e
		}
	}
	if len(s.Config.JWTSecret) < 16 {
		return errors.New("JWT secret must contain at least 16 characters")
	}
	if users == 0 {
		name := s.Config.AdminUsername
		if name == "" {
			name = "admin"
		}
		hash, e := bcrypt.GenerateFromPassword([]byte(s.Config.AdminPassword), bcrypt.DefaultCost)
		if e != nil {
			return e
		}
		if _, e = s.Store.Insert(ctx, "users", store.Row{"username": name, "hashed_password": string(hash), "created_at": s.authNow(), "is_otp": false}); e != nil {
			return e
		}
	}
	return nil
}
