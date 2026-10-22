package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/junglegaming/backend-challenge-go/internal/application"
)

// OIDCConfig contém endereços confiáveis da configuração, nunca do token recebido.
// JWKSURL pode usar o endereço interno do IdP; Issuer continua sendo validado.
type OIDCConfig struct{ Issuer, Audience, JWKSURL string }

type Authenticator struct {
	verifier *oidc.IDTokenVerifier
	client   *http.Client
	jwksURL  string
}

func NewAuthenticator(ctx context.Context, config OIDCConfig) (*Authenticator, error) {
	for _, address := range []string{config.Issuer, config.JWKSURL} {
		u, err := url.Parse(address)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
			return nil, errors.New("invalid OIDC configuration")
		}
	}
	if config.Audience == "" {
		return nil, errors.New("OIDC audience is required")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(ctx, client), config.JWKSURL)
	return &Authenticator{client: client, jwksURL: config.JWKSURL, verifier: oidc.NewVerifier(config.Issuer, keys, &oidc.Config{
		ClientID: config.Audience, SupportedSigningAlgs: []string{"RS256"},
	})}, nil
}

type principal struct {
	actor      application.Actor
	internal   bool
	providerID string
}
type principalKey struct{}

func (a *Authenticator) authenticate(ctx context.Context, raw string) (principal, error) {
	token, err := a.verifier.Verify(ctx, raw)
	if err != nil {
		return principal{}, application.ErrUnauthenticated
	}
	var claims struct {
		Subject     string `json:"sub"`
		Type        string `json:"typ"`
		ProviderID  string `json:"provider_id"`
		RealmAccess struct {
			Roles []string `json:"roles"`
		} `json:"realm_access"`
	}
	if err := token.Claims(&claims); err != nil || claims.Subject == "" || claims.Type != "Bearer" {
		return principal{}, application.ErrUnauthenticated
	}
	internal, provider := false, false
	for _, role := range claims.RealmAccess.Roles {
		internal = internal || role == "wallet-internal"
		provider = provider || role == "provider"
	}
	// Identidades ambíguas não ganham privilégios internos por precedência.
	if internal && !provider && claims.ProviderID == "" {
		return principal{actor: application.InternalActor(), internal: true}, nil
	}
	if provider && !internal {
		actor, err := application.NewProviderActor(claims.ProviderID)
		if err == nil {
			return principal{actor: actor, providerID: claims.ProviderID}, nil
		}
	}
	return principal{}, application.ErrForbidden
}

func (a *Authenticator) protect(internalOnly bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values("Authorization")
		if len(values) != 1 {
			writeError(w, application.ErrUnauthenticated)
			return
		}
		parts := strings.Fields(values[0])
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			writeError(w, application.ErrUnauthenticated)
			return
		}
		identity, err := a.authenticate(r.Context(), parts[1])
		if err != nil {
			writeError(w, err)
			return
		}
		if internalOnly && !identity.internal {
			writeError(w, application.ErrForbidden)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, identity)))
	}
}

func identityOf(r *http.Request) principal { return r.Context().Value(principalKey{}).(principal) }

func (a *Authenticator) Check(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.jwksURL, nil)
	if err != nil {
		return errors.New("invalid IdP configuration")
	}
	response, err := a.client.Do(request)
	if err != nil {
		return errors.New("IdP unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("IdP key endpoint unavailable")
	}
	var document struct {
		Keys []struct{ Kty, Kid, N, E string } `json:"keys"`
	}
	if json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&document) != nil {
		return errors.New("invalid IdP key document")
	}
	for _, key := range document.Keys {
		if key.Kty == "RSA" && key.Kid != "" && key.N != "" && key.E != "" {
			return nil
		}
	}
	return errors.New("IdP signing key unavailable")
}
