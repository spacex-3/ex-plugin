package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	openAITokenURL = "https://auth.openai.com/oauth/token"
	openAIClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

var acceptedAuthTypes = map[string]bool{
	"excel":       true,
	"ex-plugin":   true,
	"basispoints": true,
}

var allowedHeaderNames = map[string]bool{
	"authorization":            true,
	"chatgpt-account-id":       true,
	"user-agent":               true,
	"x-basispoints-auth-mode":  true,
	"x-openai-account-id":      true,
	"x-openai-account-user-id": true,
}

type credential struct {
	Type           string
	AccessToken    string
	RefreshToken   string
	IDToken        string
	AccountID      string
	UserID         string
	Email          string
	AuthMode       string
	ToolsVersionID string
	ExpiresAt      time.Time
	ExtraHeaders   map[string]string
	raw            map[string]any
}

func parseAuthFile(raw []byte, fileName string) (authRecord, bool, error) {
	cred, ok, err := parseCredential(raw)
	if err != nil || !ok {
		return authRecord{}, ok, err
	}
	record, err := cred.record(fileName)
	return record, true, err
}

func parseCredential(raw []byte) (credential, bool, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return credential{}, false, nil
	}
	var payload map[string]any
	if err := decodeJSONUseNumber(raw, &payload); err != nil {
		return credential{}, false, nil
	}
	authType := strings.ToLower(strings.TrimSpace(stringField(payload, "type")))
	if !acceptedAuthTypes[authType] {
		return credential{}, false, nil
	}
	cred := credential{
		Type:           providerID,
		AccessToken:    firstString(payload, "access_token", "accessToken"),
		RefreshToken:   firstString(payload, "refresh_token", "refreshToken"),
		IDToken:        firstString(payload, "id_token", "idToken"),
		AccountID:      firstString(payload, "account_id", "accountId", "chatgpt_account_id"),
		UserID:         firstString(payload, "chatgpt_user_id", "user_id"),
		Email:          firstString(payload, "email"),
		AuthMode:       firstString(payload, "auth_mode", "authMode"),
		ToolsVersionID: strings.TrimSpace(firstString(payload, "tools_version_id", "toolsVersionId")),
		ExtraHeaders:   map[string]string{},
		raw:            payload,
	}
	if headers, ok := payload["headers"].(map[string]any); ok {
		for key, value := range headers {
			text, ok := value.(string)
			if !ok {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(key))
			text = strings.TrimSpace(text)
			if allowedHeaderNames[name] && text != "" && len(text) <= 32768 {
				cred.ExtraHeaders[name] = text
			}
		}
	}
	if cred.AccessToken == "" {
		if authz := cred.ExtraHeaders["authorization"]; strings.HasPrefix(strings.ToLower(authz), "bearer ") {
			cred.AccessToken = strings.TrimSpace(authz[len("bearer "):])
		}
	}
	if cred.AccountID == "" {
		cred.AccountID = cred.ExtraHeaders["chatgpt-account-id"]
	}
	if cred.AccountID == "" {
		cred.AccountID = cred.ExtraHeaders["x-openai-account-id"]
	}
	if cred.AccountID == "" {
		cred.AccountID, cred.UserID = accountFromTokens(cred.AccessToken, cred.IDToken, cred.UserID)
	} else if cred.UserID == "" {
		_, cred.UserID = accountFromTokens(cred.AccessToken, cred.IDToken, "")
	}
	if cred.ExpiresAt.IsZero() {
		if exp := jwtExpiry(cred.AccessToken); !exp.IsZero() {
			cred.ExpiresAt = exp
		}
	}
	if stamp := firstString(payload, "expired", "expires_at", "expiresAt"); stamp != "" && cred.ExpiresAt.IsZero() {
		if parsed, err := time.Parse(time.RFC3339, stamp); err == nil {
			cred.ExpiresAt = parsed
		}
	}
	if cred.AccessToken == "" {
		return credential{}, true, fmt.Errorf("excel auth requires access_token")
	}
	if cred.AccountID == "" {
		return credential{}, true, fmt.Errorf("excel auth requires account_id or a ChatGPT access token carrying chatgpt_account_id")
	}
	if cred.AuthMode == "" {
		cred.AuthMode = loadedConfig().authMode()
	}
	if cred.ToolsVersionID == "" {
		cred.ToolsVersionID = strings.TrimSpace(loadedConfig().ToolsVersionID)
	}
	if !validToolsVersion(cred.ToolsVersionID) {
		return credential{}, true, fmt.Errorf("tools_version_id is not a valid Basispoints version id")
	}
	return cred, true, nil
}

func (c credential) record(fileName string) (authRecord, error) {
	storage, err := c.storageJSON()
	if err != nil {
		return authRecord{}, err
	}
	id := "excel-" + c.AccountID
	label := c.Email
	if label == "" {
		label = c.AccountID
	}
	if strings.TrimSpace(fileName) == "" {
		fileName = id + ".json"
	}
	return authRecord{
		Provider:         providerID,
		ID:               id,
		FileName:         fileName,
		Label:            label,
		StorageJSON:      storage,
		Metadata:         map[string]any{"type": providerID, "email": c.Email},
		Attributes:       map[string]string{"account_id": c.AccountID},
		NextRefreshAfter: c.nextRefresh(),
	}, nil
}

func (c credential) nextRefresh() time.Time {
	if c.RefreshToken == "" {
		if !c.ExpiresAt.IsZero() {
			return c.ExpiresAt.Add(-time.Minute)
		}
		return time.Now().Add(30 * time.Minute).UTC()
	}
	if !c.ExpiresAt.IsZero() {
		refreshAt := c.ExpiresAt.Add(-2 * time.Minute)
		if refreshAt.After(time.Now()) {
			return refreshAt.UTC()
		}
	}
	return time.Now().Add(time.Minute).UTC()
}

func (c credential) storageJSON() ([]byte, error) {
	payload := map[string]any{
		"type":         providerID,
		"access_token": c.AccessToken,
		"account_id":   c.AccountID,
		"auth_mode":    c.AuthMode,
	}
	if c.RefreshToken != "" {
		payload["refresh_token"] = c.RefreshToken
	}
	if c.IDToken != "" {
		payload["id_token"] = c.IDToken
	}
	if c.Email != "" {
		payload["email"] = c.Email
	}
	if c.UserID != "" {
		payload["chatgpt_user_id"] = c.UserID
	}
	if c.ToolsVersionID != "" {
		payload["tools_version_id"] = c.ToolsVersionID
	}
	if !c.ExpiresAt.IsZero() {
		payload["expired"] = c.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if len(c.ExtraHeaders) > 0 {
		payload["headers"] = c.ExtraHeaders
	}
	return marshalCompact(payload)
}

func (c credential) requestHeaders(stream bool) (map[string][]string, []string, error) {
	if c.AccessToken == "" || c.AccountID == "" {
		return nil, nil, fmt.Errorf("excel session is incomplete")
	}
	if !c.ExpiresAt.IsZero() && !c.ExpiresAt.After(time.Now().Add(5*time.Second)) && c.RefreshToken == "" {
		return nil, nil, fmt.Errorf("excel access token is expired")
	}
	accept := "application/json"
	if stream {
		accept = "text/event-stream"
	}
	mode := c.AuthMode
	if mode == "" {
		mode = defaultAuthMode
	}
	headers := map[string][]string{
		"authorization":           {"Bearer " + c.AccessToken},
		"chatgpt-account-id":      {c.AccountID},
		"x-openai-account-id":     {c.AccountID},
		"x-basispoints-auth-mode": {mode},
		"accept":                  {accept},
		"accept-encoding":         {"identity"},
		"content-type":            {"application/json"},
		"origin":                  {"https://bps.openai.com"},
		"x-openai-internal-basispoints-client-agent-profile":  {"excel"},
		"x-openai-internal-basispoints-client-editor":         {"excel"},
		"x-openai-internal-basispoints-client-host":           {"office"},
		"x-openai-internal-basispoints-client-platform":       {"excel"},
		"x-openai-internal-basispoints-client-platform-class": {"PC"},
		"x-openai-internal-basispoints-client-product":        {"basispoints-excel-plugin"},
		"x-openai-internal-basispoints-client-runtime":        {"desktop"},
		"x-openai-internal-basispoints-office-host":           {"Excel"},
		"x-openai-internal-basispoints-office-platform":       {"PC"},
		"x-stainless-arch":            {"unknown"},
		"x-stainless-lang":            {"js"},
		"x-stainless-os":              {"Unknown"},
		"x-stainless-package-version": {"6.31.0"},
		"x-stainless-retry-count":     {"0"},
		"x-stainless-runtime":         {"browser:chrome"},
	}
	if c.UserID != "" {
		headers["x-openai-account-user-id"] = []string{c.UserID}
	}
	for name, value := range c.ExtraHeaders {
		if name == "authorization" || name == "chatgpt-account-id" || name == "x-openai-account-id" {
			continue
		}
		headers[name] = []string{value}
	}
	order := []string{
		"authorization",
		"chatgpt-account-id",
		"x-openai-account-id",
		"x-basispoints-auth-mode",
		"content-type",
		"accept",
		"accept-encoding",
		"origin",
		"x-openai-internal-basispoints-client-product",
		"x-openai-internal-basispoints-office-host",
		"user-agent",
	}
	return headers, order, nil
}

func refreshCredential(raw []byte, fileName string) (authRecord, error) {
	cred, ok, err := parseCredential(raw)
	if err != nil {
		return authRecord{}, err
	}
	if !ok {
		return authRecord{}, fmt.Errorf("excel auth refresh did not recognize the credential")
	}
	if cred.RefreshToken == "" {
		if !cred.ExpiresAt.IsZero() && !cred.ExpiresAt.After(time.Now()) {
			return authRecord{}, fmt.Errorf("excel access token is expired and no refresh_token is stored")
		}
		return cred.record(fileName)
	}
	refreshed, err := refreshAccessToken(cred.RefreshToken)
	if err != nil {
		return authRecord{}, err
	}
	cred.AccessToken = refreshed.AccessToken
	if refreshed.RefreshToken != "" {
		cred.RefreshToken = refreshed.RefreshToken
	}
	if refreshed.IDToken != "" {
		cred.IDToken = refreshed.IDToken
	}
	if refreshed.AccountID != "" {
		cred.AccountID = refreshed.AccountID
	}
	if refreshed.Email != "" {
		cred.Email = refreshed.Email
	}
	if refreshed.ExpiresIn > 0 {
		cred.ExpiresAt = time.Now().Add(time.Duration(refreshed.ExpiresIn) * time.Second)
	} else if exp := jwtExpiry(cred.AccessToken); !exp.IsZero() {
		cred.ExpiresAt = exp
	}
	return cred.record(fileName)
}

type refreshedToken struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	AccountID    string
	Email        string
	ExpiresIn    int
}

func refreshAccessToken(refreshToken string) (refreshedToken, error) {
	form := url.Values{}
	form.Set("client_id", openAIClientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("scope", "openid profile email")
	body := []byte(form.Encode())
	raw, err := doHost(hostHTTPDo, map[string]any{
		"method": http.MethodPost,
		"url":    openAITokenURL,
		"headers": map[string][]string{
			"content-type": {"application/x-www-form-urlencoded"},
			"accept":       {"application/json"},
		},
		"body": body,
	})
	if err != nil {
		return refreshedToken{}, err
	}
	var resp struct {
		StatusCode int                 `json:"status_code"`
		Body       json.RawMessage     `json:"body"`
		Headers    map[string][]string `json:"headers"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return refreshedToken{}, err
	}
	if resp.StatusCode != 0 && resp.StatusCode != http.StatusOK {
		return refreshedToken{}, fmt.Errorf("token refresh failed with status %d: %s", resp.StatusCode, truncate(string(resp.Body), 300))
	}
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(resp.Body, &token); err != nil {
		return refreshedToken{}, fmt.Errorf("token refresh response was not json: %w", err)
	}
	if token.AccessToken == "" {
		return refreshedToken{}, fmt.Errorf("token refresh response did not include access_token")
	}
	accountID, _ := accountFromTokens(token.AccessToken, token.IDToken, "")
	email := jwtEmail(token.IDToken)
	return refreshedToken{
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		IDToken:      token.IDToken,
		AccountID:    accountID,
		Email:        email,
		ExpiresIn:    token.ExpiresIn,
	}, nil
}

func accountFromTokens(accessToken, idToken, userID string) (string, string) {
	for _, token := range []string{accessToken, idToken} {
		claims := jwtClaims(token)
		if claims == nil {
			continue
		}
		auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
		if account := stringField(auth, "chatgpt_account_id"); account != "" {
			if userID == "" {
				userID = stringField(auth, "chatgpt_user_id")
			}
			return account, userID
		}
		if account := stringField(claims, "chatgpt_account_id"); account != "" {
			return account, userID
		}
	}
	return "", userID
}

func jwtEmail(token string) string {
	claims := jwtClaims(token)
	if claims == nil {
		return ""
	}
	return stringField(claims, "email")
}

func jwtExpiry(token string) time.Time {
	claims := jwtClaims(token)
	if claims == nil {
		return time.Time{}
	}
	switch exp := claims["exp"].(type) {
	case json.Number:
		seconds, err := exp.Int64()
		if err != nil {
			return time.Time{}
		}
		return time.Unix(seconds, 0)
	case float64:
		return time.Unix(int64(exp), 0)
	default:
		return time.Time{}
	}
}

func jwtClaims(token string) map[string]any {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		token = strings.TrimSpace(token[len("bearer "):])
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	decoded, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := decodeJSONUseNumber(decoded, &claims); err != nil {
		return nil
	}
	return claims
}

func validToolsVersion(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 160 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func firstString(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := object[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

type authRecord struct {
	Provider         string            `json:"Provider"`
	ID               string            `json:"ID"`
	FileName         string            `json:"FileName"`
	Label            string            `json:"Label"`
	StorageJSON      []byte            `json:"StorageJSON"`
	Metadata         map[string]any    `json:"Metadata,omitempty"`
	Attributes       map[string]string `json:"Attributes,omitempty"`
	NextRefreshAfter time.Time         `json:"NextRefreshAfter"`
}
