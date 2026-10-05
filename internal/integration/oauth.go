// SPDX-License-Identifier: AGPL-3.0-or-later
package integration

import (
	"context"
	"net/url"
	"time"
)

type OAuthToken struct {
	AccessToken, RefreshToken, TokenType string
	ExpiresIn                            int64
	CreatedAt                            int64
}

func tokenFrom(v any, provider string, old Credential) (OAuthToken, error) {
	o := object(v)
	t := OAuthToken{AccessToken: str(o["access_token"]), RefreshToken: first(o["refresh_token"], old.RefreshToken), TokenType: first(o["token_type"], "Bearer"), ExpiresIn: int64(integer(o["expires_in"])), CreatedAt: int64(integer(o["created_at"]))}
	if t.AccessToken == "" {
		return t, &Error{provider, 502, "OAuth response omitted access token"}
	}
	if t.ExpiresIn < 0 || t.ExpiresIn > 3650*24*60*60 {
		return t, &Error{provider, 502, "invalid OAuth expiry"}
	}
	if t.CreatedAt == 0 {
		t.CreatedAt = time.Now().Unix()
	}
	return t, nil
}
func (c *Client) ExchangeBangumi(ctx context.Context, code string, cred Credential) (OAuthToken, error) {
	if code == "" || cred.ClientID == "" || cred.ClientSecret == "" || cred.RedirectURI == "" {
		return OAuthToken{}, &Error{"bangumi", 412, "OAuth configuration incomplete"}
	}
	v, e := c.json(ctx, "bangumi", "POST", c.base(ctx, "bangumiOAuthBaseUrl", "https://bgm.tv")+"/oauth/access_token", nil, nil, url.Values{"grant_type": {"authorization_code"}, "client_id": {cred.ClientID}, "client_secret": {cred.ClientSecret}, "redirect_uri": {cred.RedirectURI}, "code": {code}})
	if e != nil {
		return OAuthToken{}, e
	}
	return tokenFrom(v, "bangumi", cred)
}
func (c *Client) RefreshOAuth(ctx context.Context, provider string, cred Credential) (OAuthToken, error) {
	if cred.RefreshToken == "" {
		return OAuthToken{}, &Error{provider, 412, "refresh token missing"}
	}
	var v any
	var e error
	switch provider {
	case "bangumi":
		if cred.ClientID == "" || cred.ClientSecret == "" || cred.RedirectURI == "" {
			return OAuthToken{}, &Error{provider, 412, "OAuth configuration incomplete"}
		}
		v, e = c.json(ctx, provider, "POST", c.base(ctx, "bangumiOAuthBaseUrl", "https://bgm.tv")+"/oauth/access_token", nil, nil, url.Values{"grant_type": {"refresh_token"}, "client_id": {cred.ClientID}, "client_secret": {cred.ClientSecret}, "refresh_token": {cred.RefreshToken}, "redirect_uri": {cred.RedirectURI}})
	case "trakt":
		v, e = c.json(ctx, provider, "POST", c.base(ctx, "traktOAuthWorkerUrl", "https://danmu-api.misaka10876.top")+"/oauth/refresh", nil, nil, map[string]any{"provider": "trakt", "refresh_token": cred.RefreshToken})
	default:
		return OAuthToken{}, &Error{provider, 400, "OAuth provider unsupported"}
	}
	if e != nil {
		return OAuthToken{}, e
	}
	return tokenFrom(v, provider, cred)
}
func (c *Client) BangumiProfile(ctx context.Context, access string) (map[string]any, error) {
	v, e := c.json(ctx, "bangumi", "GET", c.base(ctx, "bangumiApiBaseUrl", "https://api.bgm.tv")+"/v0/me", nil, map[string]string{"Authorization": "Bearer " + access}, nil)
	return object(v), e
}
func (c *Client) TraktProfile(ctx context.Context, cred Credential) (map[string]any, error) {
	h, e := c.traktHeaders(ctx, cred, false)
	if e != nil {
		return nil, e
	}
	v, e := c.json(ctx, "trakt", "GET", c.base(ctx, "traktApiBaseUrl", "https://api.trakt.tv")+"/users/me", nil, h, nil)
	return object(v), e
}
