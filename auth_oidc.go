package sam

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	openaiResource       = "https://api.openai.com/v1"
	openaiTokenEndpoint  = openaiIssuer + "/api/accounts/oauth/token"
	openaiRevokeEndpoint = openaiIssuer + "/api/accounts/oauth/revoke"
	openaiJWKS           = openaiIssuer + "/.well-known/jwks.json"
	openaiScopes         = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
)

type identityClaims struct {
	Issuer          string          `json:"iss"`
	Subject         string          `json:"sub"`
	Audience        json.RawMessage `json:"aud"`
	AuthorizedParty json.RawMessage `json:"azp"`
	Email           string          `json:"email"`
	Nonce           string          `json:"nonce"`
	Expires         *int64          `json:"exp"`
	Issued          *int64          `json:"iat"`
	NotBefore       json.RawMessage `json:"nbf"`
}

// validateID always resolves keys afresh from the fixed trusted origin. JWT
// jku/x5u/issuer values never select a network target or an algorithm.
func (m *AuthManager) validateID(ctx context.Context, token, client, nonce string, historical bool) (registration, error) {
	bad := errorf(CodeAuthRequired, "OpenAI ID token failed signature, identity or time validation; start a fresh authorization")
	parts := strings.Split(token, ".")
	if len(token) > maxAuthBody || len(parts) != 3 || client == "" || client == "dynamic_agent_client" {
		return registration{}, bad
	}
	var head struct {
		Alg  string   `json:"alg"`
		Kid  string   `json:"kid"`
		Crit []string `json:"crit"`
	}
	if decodeJWTPart(parts[0], &head) != nil || head.Alg != "RS256" || head.Kid == "" || len(head.Crit) != 0 {
		return registration{}, bad
	}
	var claims identityClaims
	if decodeJWTPart(parts[1], &claims) != nil || !validIDClaims(claims, client, nonce, historical) {
		return registration{}, bad
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return registration{}, bad
	}
	key, err := m.signingKey(ctx, head.Kid)
	if err != nil {
		return registration{}, err
	}
	hash := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, hash[:], sig) != nil {
		return registration{}, bad
	}
	return registration{ClientID: client, Issuer: claims.Issuer, Subject: claims.Subject, Email: claims.Email, RedirectScheme: "http", RedirectHost: "127.0.0.1", RedirectPath: "/auth/callback"}, nil
}

func decodeJWTPart(part string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		return err
	}
	if !utf8.Valid(b) {
		return fmt.Errorf("invalid JWT UTF-8")
	}
	// Reject duplicate top-level JOSE/claim names instead of interpreting an
	// ambiguous signed object differently from another OIDC implementation.
	d := json.NewDecoder(bytes.NewReader(b))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return fmt.Errorf("invalid JWT object")
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return fmt.Errorf("duplicate JWT field")
		}
		seen[name] = true
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return err
		}
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JWT data")
	}
	return json.Unmarshal(b, v)
}

func validIDClaims(c identityClaims, client, nonce string, historical bool) bool {
	if c.Issuer != openaiIssuer || strings.TrimSpace(c.Subject) == "" || c.Expires == nil || c.Issued == nil {
		return false
	}
	now := time.Now().Unix()
	if *c.Issued > now || *c.Expires <= *c.Issued || (!historical && *c.Expires <= now) {
		return false
	}
	if len(c.NotBefore) > 0 {
		var n int64
		if string(c.NotBefore) == "null" || json.Unmarshal(c.NotBefore, &n) != nil || n > now {
			return false
		}
	}
	if nonce != "" && c.Nonce != nonce {
		return false
	}
	return validIDAudience(c, client)
}

func validIDAudience(c identityClaims, client string) bool {
	var audience []string
	var single string
	if json.Unmarshal(c.Audience, &single) == nil && single != "" {
		audience = []string{single}
	} else if json.Unmarshal(c.Audience, &audience) != nil {
		return false
	}
	if !slices.Contains(audience, client) || (len(audience) > 1 && len(c.AuthorizedParty) == 0) {
		return false
	}
	if len(c.AuthorizedParty) > 0 {
		var party string
		if json.Unmarshal(c.AuthorizedParty, &party) != nil || party != client {
			return false
		}
	}
	return true
}

type signingJWK struct {
	Type string   `json:"kty"`
	ID   string   `json:"kid"`
	Use  string   `json:"use"`
	Alg  string   `json:"alg"`
	Ops  []string `json:"key_ops"`
	N    string   `json:"n"`
	E    string   `json:"e"`
}

func (m *AuthManager) signingKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	resp, err := m.send(ctx, http.MethodGet, openaiJWKS, "", "", nil)
	if err != nil {
		return nil, err
	}
	if resp.status != http.StatusOK {
		return nil, resp.failure("OpenAI signing-key lookup")
	}
	var set struct {
		Keys []signingJWK `json:"keys"`
	}
	bad := errorf(CodeAuthRequired, "OpenAI signing key is missing, ambiguous or malformed")
	if json.Unmarshal(resp.raw, &set) != nil || len(set.Keys) == 0 || len(set.Keys) > 128 {
		return nil, bad
	}
	var found *rsa.PublicKey
	for _, j := range set.Keys {
		if j.ID != kid {
			continue
		}
		if found != nil {
			return nil, bad
		}
		found = publicRSA(j)
		if found == nil {
			return nil, bad
		}
	}
	if found == nil {
		return nil, bad
	}
	return found, nil
}

func publicRSA(j signingJWK) *rsa.PublicKey {
	if j.Type != "RSA" || (j.Alg != "" && j.Alg != "RS256") || (j.Use != "" && j.Use != "sig") || (len(j.Ops) > 0 && !slices.Contains(j.Ops, "verify")) {
		return nil
	}
	nb, err := base64.RawURLEncoding.DecodeString(j.N)
	if err != nil || len(nb) > 1024 || len(nb) == 0 || nb[0] == 0 {
		return nil
	}
	eb, err := base64.RawURLEncoding.DecodeString(j.E)
	if err != nil || len(eb) == 0 || len(eb) > 4 || eb[0] == 0 {
		return nil
	}
	n := new(big.Int).SetBytes(nb)
	var e uint64
	for _, b := range eb {
		e = e<<8 | uint64(b)
	}
	if n.BitLen() < 2048 || n.Bit(0) != 1 || e < 3 || e > 1<<31-1 || e%2 == 0 {
		return nil
	}
	return &rsa.PublicKey{N: n, E: int(e)}
}

func sameRegistration(a, b registration) bool {
	return a.ClientID == b.ClientID && a.Issuer == b.Issuer && a.Subject == b.Subject
}
