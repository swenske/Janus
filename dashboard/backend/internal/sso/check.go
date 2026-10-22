package sso

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/go-jose/go-jose/v4"
)

// Report is what checking a provider's settings found: its discovery
// document and keys, and what in the settings won't work with them.
type Report struct {
	// Error is what stopped the check (nothing below it is known).
	Error string `json:"error,omitempty"`
	// Issuer is the discovery document's, to copy as is; IssuerMatches,
	// the settings say exactly that.
	Issuer        string   `json:"issuer,omitempty"`
	IssuerMatches bool     `json:"issuer_matches"`
	Metadata      Metadata `json:"metadata"`
	// Keys are the provider's signing keys; KeyAlgorithms theirs.
	Keys          int      `json:"keys"`
	KeyAlgorithms []string `json:"key_algorithms"`
	// TokenAuth is how the Controller will authenticate at the token
	// endpoint.
	TokenAuth string `json:"token_auth,omitempty"`
	// Warnings are what to fix or to know before signing in.
	Warnings []string `json:"warnings"`
}

// Check fetches p's discovery document and keys, the way a sign-in will,
// and says what in p won't work with them. hasSecret: p has a client
// secret.
func Check(ctx context.Context, p Provider, hasSecret bool) Report {
	r := Report{Warnings: []string{}, KeyAlgorithms: []string{}}
	if err := checkIssuer(p.Issuer); err != nil {
		r.Error = err.Error()
		return r
	}
	if err := p.TLS.tls().Validate(); err != nil {
		r.Error = "tls: " + err.Error()
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, 2*requestTimeout)
	defer cancel()
	hc := HTTPClient(p.TLS)
	if err := getJSON(ctx, hc, strings.TrimSuffix(p.Issuer, "/")+"/.well-known/openid-configuration", &r.Metadata); err != nil {
		r.Error = "discovery: " + err.Error()
		return r
	}
	m := r.Metadata
	r.Issuer, r.IssuerMatches = m.Issuer, m.Issuer == p.Issuer
	if !r.IssuerMatches {
		r.Warnings = append(r.Warnings, fmt.Sprintf("The discovery document's issuer is %q: set the issuer exactly so - an ID token from %q is refused.", m.Issuer, p.Issuer))
	}
	if err := m.checkEndpoints(); err != nil {
		r.Error = err.Error()
		return r
	}
	var keys jose.JSONWebKeySet
	if err := getJSON(ctx, hc, m.JWKSURI, &keys); err != nil {
		r.Error = "the provider's keys (" + m.JWKSURI + "): " + err.Error()
		return r
	}
	for _, k := range keys.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		r.Keys++
		if k.Algorithm != "" && !slices.Contains(r.KeyAlgorithms, k.Algorithm) {
			r.KeyAlgorithms = append(r.KeyAlgorithms, k.Algorithm)
		}
	}
	if r.Keys == 0 {
		r.Warnings = append(r.Warnings, "The provider publishes no signing key: no ID token can be checked.")
	}
	if _, style, err := authStyle(m.TokenEndpointAuthMethods, hasSecret); err != nil {
		r.Warnings = append(r.Warnings, err.Error()+".")
	} else {
		r.TokenAuth = style
	}
	if !hasSecret {
		r.Warnings = append(r.Warnings, "No client secret: the Controller signs in as a public client, bound by PKCE alone.")
	}
	if len(m.ScopesSupported) > 0 {
		for _, s := range p.Scopes {
			if !slices.Contains(m.ScopesSupported, s) {
				r.Warnings = append(r.Warnings, fmt.Sprintf("The provider doesn't list the scope %q (it lists %s): it may refuse the sign-in.", s, strings.Join(m.ScopesSupported, ", ")))
			}
		}
	}
	if len(m.ClaimsSupported) > 0 {
		for _, c := range []string{p.UsernameClaim, p.GroupsClaim} {
			top, _, _ := strings.Cut(c, ".")
			if !slices.Contains(m.ClaimsSupported, c) && !slices.Contains(m.ClaimsSupported, top) {
				r.Warnings = append(r.Warnings, fmt.Sprintf("The provider doesn't list the claim %q - a mapper's (Keycloak's groups) aren't listed: check that its tokens carry it.", c))
			}
		}
	}
	if !p.DisablePKCE && !slices.Contains(m.CodeChallengeMethods, "S256") {
		r.Warnings = append(r.Warnings, "The provider doesn't say it supports PKCE (S256): if signing in fails at the code exchange, turn PKCE off.")
	}
	if m.EndSessionEndpoint == "" && !p.LocalLogout {
		r.Warnings = append(r.Warnings, "The provider publishes no end_session_endpoint: signing out of the Controller leaves its session at the provider open.")
	}
	return r
}

func getJSON(ctx context.Context, hc *http.Client, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("not JSON: %w", err)
	}
	return nil
}
