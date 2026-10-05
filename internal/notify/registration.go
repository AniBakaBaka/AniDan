// SPDX-License-Identifier: AGPL-3.0-only
package notify

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

var commandRE = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
var webhookSecretRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

// RegisterCommands replaces the Telegram or WeCom bot's command menu only when an
// operator explicitly invokes it. Startup, Poll, Send and Test never call it.
// Empty commands explicitly clears a Telegram menu; WeCom requires 1-15 entries.
// SC3 native menu setup is unsupported; conversation choices are numbered text.
func (s *Service) RegisterCommands(ctx context.Context, c Channel, commands []Command) error {
	if err := Validate(c); err != nil {
		return err
	}
	if c.Type != "telegram" && c.Type != "wechat" {
		return fmt.Errorf("%w: native command registration", ErrUnsupported)
	}
	if len(commands) > 100 {
		return ErrLimit
	}
	seen := make(map[string]bool, len(commands))
	clean := make([]Command, len(commands))
	for i, command := range commands {
		command.Command = strings.TrimPrefix(command.Command, "/")
		if !commandRE.MatchString(command.Command) || seen[command.Command] || !utf8.ValidString(command.Description) || len([]rune(command.Description)) < 1 || len([]rune(command.Description)) > 256 {
			return errors.New("invalid or duplicate bot command")
		}
		seen[command.Command] = true
		clean[i] = command
	}
	if c.Type == "wechat" {
		return s.registerWeCommands(ctx, c, clean)
	}
	return s.botSetting(ctx, c, "setMyCommands", map[string]any{"commands": clean})
}

func (s *Service) registerWeCommands(ctx context.Context, c Channel, commands []Command) error {
	var routeErr error
	ctx, routeErr = s.withRoute(ctx, c)
	if routeErr != nil {
		return routeErr
	}
	// WeCom supports three root buttons with up to five child click buttons.
	// Always group entries so descriptions use the 60-byte child-label limit.
	if len(commands) == 0 || len(commands) > 15 {
		return ErrLimit
	}
	groups := make([]map[string]any, 0, 3)
	for i := 0; i < len(commands); i += 5 {
		children := make([]map[string]string, 0, 5)
		for _, command := range commands[i:min(i+5, len(commands))] {
			if len(command.Description) > 60 {
				return errors.New("WeCom menu descriptions must not exceed 60 UTF-8 bytes")
			}
			children = append(children, map[string]string{"type": "click", "name": command.Description, "key": command.Command})
		}
		groups = append(groups, map[string]any{"name": fmt.Sprintf("Commands %d", i/5+1), "sub_button": children})
	}
	token, err := s.weToken(ctx, c, false)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		endpoint := "https://qyapi.weixin.qq.com/cgi-bin/menu/create?" + url.Values{"access_token": {token}, "agentid": {Str(c.Config["agent_id"])}}.Encode()
		v, err := s.request(ctx, c, "POST", endpoint, map[string]any{"button": groups})
		if err != nil {
			return err
		}
		if v["errcode"] != nil && zeroCode(v["errcode"]) {
			return nil
		}
		if attempt == 0 && num(v["errcode"]) == 42001 {
			token, err = s.weToken(ctx, c, true)
			if err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("WeCom menu registration rejected (code %s)", responseCode(v["errcode"]))
	}
	return errors.New("WeCom menu registration failed")
}

// RegisterWebhook registers the exact operator-selected callback URL. It never
// probes/fetches the URL, removes a previous webhook first, or drops updates.
// Query values may contain the server's API key and are never returned in errors.
func (s *Service) RegisterWebhook(ctx context.Context, c Channel, callbackURL string) error {
	if err := Validate(c); err != nil {
		return err
	}
	if c.Type != "telegram" {
		return fmt.Errorf("%w: automatic webhook registration", ErrUnsupported)
	}
	if mode := Str(c.Config["mode"]); mode != "" && mode != "webhook" {
		return errors.New("webhook registration requires webhook mode")
	}
	u, err := url.Parse(callbackURL)
	if err != nil || len(callbackURL) > 4096 || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Host == "" || (u.Port() != "" && u.Port() != "443") || u.Path == "" || strings.ContainsAny(callbackURL, "\x00\r\n") {
		return errors.New("webhook URL must be a public HTTPS callback URL")
	}
	if _, err = PublicDomain("https://" + u.Host); err != nil {
		return errors.New("webhook URL must be a public HTTPS callback URL")
	}
	payload := map[string]any{"url": callbackURL, "allowed_updates": []string{"message", "callback_query"}, "drop_pending_updates": false}
	if secret := Str(c.Config["webhook_secret"]); secret != "" {
		if !webhookSecretRE.MatchString(secret) {
			return errors.New("Telegram webhook secret must use 1 to 256 letters, digits, underscores or hyphens")
		}
		payload["secret_token"] = secret
	}
	return s.botSetting(ctx, c, "setWebhook", payload)
}

// DeleteWebhook is an explicit setup step before switching a registered
// Telegram bot to polling. Pending updates are preserved.
func (s *Service) DeleteWebhook(ctx context.Context, c Channel) error {
	if err := Validate(c); err != nil {
		return err
	}
	if c.Type != "telegram" {
		return fmt.Errorf("%w: webhook removal", ErrUnsupported)
	}
	return s.botSetting(ctx, c, "deleteWebhook", map[string]bool{"drop_pending_updates": false})
}

func (s *Service) botSetting(ctx context.Context, c Channel, method string, payload any) error {
	v, err := s.request(ctx, c, "POST", s.botURL(c, method), payload)
	if err != nil {
		return err
	}
	if err = botOK(v); err != nil {
		return err
	}
	if result, ok := v["result"].(bool); !ok || !result {
		return errors.New("bot API did not confirm settings change")
	}
	return nil
}
