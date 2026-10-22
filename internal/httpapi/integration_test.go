//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/httpapi"
	"github.com/junglegaming/backend-challenge-go/internal/postgres"
)

// HTTP, casos de uso, PostgreSQL e tokens/JWKS do Keycloak reais.
// Nenhuma implementação de repositório ou autenticação é substituída por mock.
type apiFixture struct {
	pool                           *pgxpool.Pool
	server                         *httptest.Server
	client                         *http.Client
	internal, providerA, providerB string
}
type jsonObject = map[string]any

func newAPI(t *testing.T) *apiFixture {
	t.Helper()
	databaseURL, issuer := os.Getenv("POSTGRES_TEST_URL"), os.Getenv("OIDC_TEST_ISSUER_URL")
	if databaseURL == "" || issuer == "" {
		t.Fatal("HTTP integration requires POSTGRES_TEST_URL and OIDC_TEST_ISSUER_URL; run scripts/test-http-integration.sh")
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("POSTGRES_TEST_URL must be a PostgreSQL URL")
	}
	admin, err := postgres.Open(t.Context(), databaseURL)
	if err != nil {
		t.Fatal("connect to integration PostgreSQL:", err)
	}
	t.Cleanup(admin.Close)
	name := "http_test_" + strings.ToLower(rand.Text())
	if _, err := admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop temporary HTTP database: %v", err)
		}
	})
	parsed.Path, parsed.RawPath = "/"+name, ""
	pool, err := postgres.Open(t.Context(), parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var actual string
	if err := pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&actual); err != nil || actual != name {
		t.Fatal("refusing to migrate a database not created by this test")
	}
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "000001_initial.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), string(migration)); err != nil {
		t.Fatal(err)
	}
	manager, err := postgres.NewTransactor(pool)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	deps := application.Dependencies{Transactions: manager, Logger: logger}
	wallets, err := application.NewWalletService(deps)
	if err != nil {
		t.Fatal(err)
	}
	wagers, err := application.NewWageringService(deps)
	if err != nil {
		t.Fatal(err)
	}
	jwks := os.Getenv("OIDC_TEST_JWKS_URL")
	if jwks == "" {
		jwks = issuer + "/protocol/openid-connect/certs"
	}
	auth, err := httpapi.NewAuthenticator(t.Context(), httpapi.OIDCConfig{Issuer: issuer, Audience: "wallet-api", JWKSURL: jwks})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewRouter(httpapi.Config{Wallets: wallets, Wagering: wagers, Auth: auth, CheckPostgres: pool.Ping, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	f := &apiFixture{pool: pool, server: server, client: &http.Client{Timeout: 10 * time.Second}}
	f.internal = f.token(t, issuer, "wallet-service")
	f.providerA = f.token(t, issuer, "provider-a")
	f.providerB = f.token(t, issuer, "provider-b")
	return f
}
func (f *apiFixture) token(t *testing.T, issuer, clientID string) string {
	t.Helper()
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientID + "-local-secret"}}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, issuer+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := f.client.Do(request)
	if err != nil {
		t.Fatal("request real Keycloak token:", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("Keycloak token request for %s returned %d", clientID, response.StatusCode)
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&token); err != nil || token.AccessToken == "" {
		t.Fatal("Keycloak did not return an access token")
	}
	return token.AccessToken
}
func (f *apiFixture) call(t *testing.T, method, path, token, key string, payload any, status int) jsonObject {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, f.server.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := f.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result jsonObject
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatalf("%s %s: decode JSON response: %v", method, path, err)
	}
	if response.StatusCode != status {
		t.Fatalf("%s %s: status=%d want=%d response=%v", method, path, response.StatusCode, status, result)
	}
	if response.Header.Get("Content-Type") != "application/json" || response.Header.Get("X-Correlation-ID") == "" {
		t.Fatal("missing JSON content type or correlation ID")
	}
	return result
}
func amount(t *testing.T, value any, want string) {
	t.Helper()
	money, ok := value.(map[string]any)
	if !ok || money["amount"] != want || money["currency"] != "BRL" {
		t.Fatalf("money=%v want amount string %s BRL", value, want)
	}
}
func errorCode(t *testing.T, response jsonObject, want string) {
	t.Helper()
	failure, ok := response["error"].(map[string]any)
	if !ok || failure["code"] != want {
		t.Fatalf("error=%v want=%s", response, want)
	}
}
func (f *apiFixture) counts(t *testing.T) [4]int64 {
	t.Helper()
	var result [4]int64
	if err := f.pool.QueryRow(t.Context(), `SELECT (SELECT COUNT(*) FROM wallets),
  (SELECT COUNT(*) FROM wager_transactions),(SELECT COUNT(*) FROM wallet_ledger_entries),
  (SELECT COUNT(*) FROM outbox_events)`).Scan(&result[0], &result[1], &result[2], &result[3]); err != nil {
		t.Fatal(err)
	}
	return result
}
func (f *apiFixture) unchanged(t *testing.T, want [4]int64) {
	t.Helper()
	if actual := f.counts(t); actual != want {
		t.Fatalf("financial/audit rows changed: %v want %v", actual, want)
	}
}
func (f *apiFixture) open(t *testing.T, initial string) string {
	t.Helper()
	result := f.call(t, "POST", "/wallets", f.internal, "", jsonObject{"playerId": "player-1", "initialBalance": jsonObject{"amount": initial, "currency": "BRL"}}, 201)
	amount(t, result["balance"], initial)
	if result["version"] != float64(1) {
		t.Fatal("opening version differs from 1")
	}
	id, ok := result["id"].(string)
	if !ok || id == "" {
		t.Fatal("opening did not return wallet ID")
	}
	return id
}
func wager(walletID, provider, externalID, kind, value string) jsonObject {
	return jsonObject{"providerId": provider, "externalTransactionId": externalID, "playerId": "player-1", "walletId": walletID,
		"roundId": "round-1", "gameId": "game-1", "kind": kind, "money": jsonObject{"amount": value, "currency": "BRL"}}
}
func (f *apiFixture) reconcile(t *testing.T, id, value string, entries int) {
	t.Helper()
	result := f.call(t, "POST", "/wallets/"+id+"/reconciliation", f.internal, "", nil, 200)
	amount(t, result["storedBalance"], value)
	amount(t, result["calculatedBalance"], value)
	amount(t, result["difference"], "0.00")
	if result["consistent"] != true || result["checkedEntries"] != float64(entries) {
		t.Fatalf("inconsistent reconciliation: %v", result)
	}
}

func TestHTTPWalletOpening(t *testing.T) {
	for _, initial := range []string{"100.00", "0.00"} {
		t.Run(initial, func(t *testing.T) {
			f := newAPI(t)
			walletID := f.open(t, initial)
			rows := [4]int64{1, 1, 1, 2}
			entries := 1
			if initial == "0.00" {
				rows = [4]int64{1, 0, 0, 0}
				entries = 0
			}
			f.unchanged(t, rows)
			got := f.call(t, "GET", "/wallets/"+walletID, f.internal, "", nil, 200)
			amount(t, got["balance"], initial)
			duplicate := f.call(t, "POST", "/wallets", f.internal, "", jsonObject{"playerId": "player-1", "initialBalance": jsonObject{"amount": initial, "currency": "BRL"}}, 409)
			errorCode(t, duplicate, "CONFLICT")
			f.unchanged(t, rows)
			f.reconcile(t, walletID, initial, entries)
		})
	}
}

func TestHTTPBetReplayRejectionAndReconciliation(t *testing.T) {
	f := newAPI(t)
	walletID := f.open(t, "100.00")
	bet := wager(walletID, "provider-a", "bet-1", "BET", "25.00")
	first := f.call(t, "POST", "/wagering/transactions", f.providerA, "key:bet-1", bet, 200)
	amount(t, first["balance"], "75.00")
	if first["status"] != "PROCESSED" || first["idempotentReplay"] != false {
		t.Fatalf("unexpected first result: %v", first)
	}
	f.unchanged(t, [4]int64{1, 2, 2, 4})
	f.call(t, "POST", "/wagering/transactions", f.providerA, "key:win-1", wager(walletID, "provider-a", "win-1", "WIN", "10.00"), 200)
	before := f.counts(t)
	replay := f.call(t, "POST", "/wagering/transactions", f.providerA, "key:bet-1", bet, 200)
	if replay["transactionId"] != first["transactionId"] || replay["idempotentReplay"] != true {
		t.Fatalf("replay differs: %v", replay)
	}
	amount(t, replay["balance"], "75.00") // Saldo original, apesar de o saldo atual ser 85.00.
	f.unchanged(t, before)
	changed := wager(walletID, "provider-a", "bet-1", "BET", "26.00")
	errorCode(t, f.call(t, "POST", "/wagering/transactions", f.providerA, "key:bet-1", changed, 409), "CONFLICT")
	errorCode(t, f.call(t, "POST", "/wagering/transactions", f.providerA, "another-key", bet, 409), "CONFLICT")
	f.unchanged(t, before)
	rejectedInput := wager(walletID, "provider-a", "bet-too-large", "BET", "100.00")
	rejected := f.call(t, "POST", "/wagering/transactions", f.providerA, "key:rejected", rejectedInput, 422)
	if rejected["status"] != "REJECTED" || rejected["failureCode"] != "INSUFFICIENT_FUNDS" || rejected["idempotentReplay"] != false {
		t.Fatalf("rejection was not persisted: %v", rejected)
	}
	if _, present := rejected["balance"]; present {
		t.Fatal("rejection exposed an unconfirmed balance")
	}
	f.unchanged(t, [4]int64{1, 4, 3, 7})
	replay = f.call(t, "POST", "/wagering/transactions", f.providerA, "key:rejected", rejectedInput, 422)
	if replay["transactionId"] != rejected["transactionId"] || replay["failureCode"] != "INSUFFICIENT_FUNDS" || replay["idempotentReplay"] != true {
		t.Fatal("rejection replay differs")
	}
	f.unchanged(t, [4]int64{1, 4, 3, 7})
	for _, path := range []string{"/wagering/transactions/" + rejected["transactionId"].(string), "/providers/provider-a/wagering/transactions/bet-too-large"} {
		got := f.call(t, "GET", path, f.providerA, "", nil, 200)
		if got["status"] != "REJECTED" || got["failureCode"] != "INSUFFICIENT_FUNDS" {
			t.Fatal("query lost rejection")
		}
	}
	f.reconcile(t, walletID, "85.00", 3)
}

func TestHTTPProviderIsolation(t *testing.T) {
	f := newAPI(t)
	walletID := f.open(t, "100.00")
	aInput := wager(walletID, "provider-a", "shared-id", "BET", "10.00")
	a := f.call(t, "POST", "/wagering/transactions", f.providerA, "shared-key", aInput, 200)
	bInput := wager(walletID, "provider-b", "shared-id", "BET", "10.00")
	b := f.call(t, "POST", "/wagering/transactions", f.providerB, "shared-key", bInput, 200)
	if a["transactionId"] == b["transactionId"] {
		t.Fatal("providers shared a transaction namespace")
	}
	before := f.counts(t)
	for _, tt := range []struct {
		token, otherProvider, otherID string
		payload                       jsonObject
	}{
		{f.providerA, "provider-b", b["transactionId"].(string), bInput},
		{f.providerB, "provider-a", a["transactionId"].(string), aInput},
	} {
		errorCode(t, f.call(t, "GET", "/wagering/transactions/"+tt.otherID, tt.token, "", nil, 403), "FORBIDDEN")
		errorCode(t, f.call(t, "GET", "/providers/"+tt.otherProvider+"/wagering/transactions/shared-id", tt.token, "", nil, 403), "FORBIDDEN")
		errorCode(t, f.call(t, "POST", "/wagering/transactions", tt.token, "shared-key", tt.payload, 403), "FORBIDDEN")
	}
	for _, tt := range []struct{ token, provider, id string }{{f.providerA, "provider-a", a["transactionId"].(string)}, {f.providerB, "provider-b", b["transactionId"].(string)}} {
		got := f.call(t, "GET", "/providers/"+tt.provider+"/wagering/transactions/shared-id", tt.token, "", nil, 200)
		if got["transactionId"] != tt.id || got["providerId"] != tt.provider {
			t.Fatal("query returned another provider's transaction")
		}
		f.call(t, "GET", "/wagering/transactions/"+tt.id, f.internal, "", nil, 200)
	}
	f.unchanged(t, before)
	f.reconcile(t, walletID, "80.00", 3)
}

func TestHTTPAuthenticationAndInternalPermissions(t *testing.T) {
	f := newAPI(t)
	walletID := f.open(t, "100.00")
	before := f.counts(t)
	for _, token := range []string{"", "not-a-jwt", tamperToken(t, f.providerA)} {
		for _, path := range []string{"/wallets/" + walletID, "/wagering/transactions/unknown"} {
			errorCode(t, f.call(t, "GET", path, token, "", nil, 401), "UNAUTHENTICATED")
		}
		errorCode(t, f.call(t, "POST", "/wagering/transactions", token, "attempt", wager(walletID, "provider-a", "attempt", "BET", "1.00"), 401), "UNAUTHENTICATED")
	}
	for _, token := range []string{f.providerA, f.providerB} {
		for _, path := range []string{"/wallets/" + walletID, "/wallets/" + walletID + "/ledger"} {
			errorCode(t, f.call(t, "GET", path, token, "", nil, 403), "FORBIDDEN")
		}
		errorCode(t, f.call(t, "POST", "/wallets/"+walletID+"/reconciliation", token, "", nil, 403), "FORBIDDEN")
		errorCode(t, f.call(t, "POST", "/wallets", token, "", jsonObject{"playerId": "unauthorized", "initialBalance": jsonObject{"amount": "1.00", "currency": "BRL"}}, 403), "FORBIDDEN")
	}
	errorCode(t, f.call(t, "POST", "/wagering/transactions", f.internal, "internal-bet", wager(walletID, "provider-a", "internal-bet", "BET", "1.00"), 403), "FORBIDDEN")
	for _, path := range []string{"/health/live", "/health/ready"} {
		f.call(t, "GET", path, "", "", nil, 200)
	}
	f.unchanged(t, before)
	f.reconcile(t, walletID, "100.00", 1)
}

func TestHTTPRejectsExpiredKeycloakToken(t *testing.T) {
	f := newAPI(t)
	token := f.token(t, os.Getenv("OIDC_TEST_ISSUER_URL"), "provider-expired-test")
	expiresAt := tokenExpiry(t, token)
	remaining := time.Until(expiresAt)
	if remaining <= 0 || remaining > 20*time.Second {
		t.Fatalf("Keycloak test token expires in %s; want a short-lived token with at most 20 seconds remaining", remaining)
	}

	path := "/providers/provider-a/wagering/transactions/expired-token-probe"
	f.call(t, http.MethodGet, path, token, "", nil, http.StatusNotFound)

	wait := time.Until(expiresAt.Add(time.Second))
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-t.Context().Done():
			t.Fatal("test canceled while waiting for the Keycloak token to expire")
		}
	}

	errorCode(t, f.call(t, http.MethodGet, path, token, "", nil, http.StatusUnauthorized), "UNAUTHENTICATED")
}

func tokenExpiry(t *testing.T, token string) time.Time {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("Keycloak did not return a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal("decode Keycloak token claims:", err)
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.ExpiresAt <= 0 {
		t.Fatal("Keycloak token does not contain a valid exp claim")
	}
	return time.Unix(claims.ExpiresAt, 0)
}

func tamperToken(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[2]) < 2 {
		t.Fatal("real IdP token is not a signed JWT")
	}
	replacement := "A"
	if parts[2][:1] == replacement {
		replacement = "B"
	}
	parts[2] = replacement + parts[2][1:]
	return strings.Join(parts, ".")
}

func TestHTTPInvalidInputAndPendingReference(t *testing.T) {
	f := newAPI(t)
	walletID := f.open(t, "100.00")
	before := f.counts(t)
	for _, tt := range []struct {
		kind  string
		value any
		key   string
	}{
		{"OPENING", "1.00", "invalid-opening"}, {"BET", 1.25, "numeric-money"},
		{"BET", "1.2", "invalid-scale"}, {"BET", "1.00", ""},
	} {
		input := wager(walletID, "provider-a", "invalid", "BET", "1.00")
		input["kind"] = tt.kind
		input["money"] = jsonObject{"amount": tt.value, "currency": "BRL"}
		errorCode(t, f.call(t, "POST", "/wagering/transactions", f.providerA, tt.key, input, 400), "INVALID_INPUT")
	}
	f.unchanged(t, before)
	pendingInput := wager(walletID, "provider-a", "early-refund", "REFUND", "5.00")
	pendingInput["referenceExternalTransactionId"] = "bet-not-arrived"
	pending := f.call(t, "POST", "/wagering/transactions", f.providerA, "early-refund", pendingInput, 202)
	if pending["status"] != "PENDING_REFERENCE" || pending["idempotentReplay"] != false {
		t.Fatalf("unexpected pending result: %v", pending)
	}
	if _, present := pending["balance"]; present {
		t.Fatal("pending reference exposed an unconfirmed balance")
	}
	f.unchanged(t, [4]int64{1, 2, 1, 3})
	replay := f.call(t, "POST", "/wagering/transactions", f.providerA, "early-refund", pendingInput, 202)
	if replay["idempotentReplay"] != true || replay["transactionId"] != pending["transactionId"] {
		t.Fatal("pending replay differs")
	}
	f.unchanged(t, [4]int64{1, 2, 1, 3})
	f.call(t, "GET", "/wagering/transactions/"+pending["transactionId"].(string), f.providerA, "", nil, 200)
	f.reconcile(t, walletID, "100.00", 1)
	// Percorrer o cursor de uma abertura comprova o formato HTTP da paginação.
	page := f.call(t, "GET", fmt.Sprintf("/wallets/%s/ledger?limit=1", walletID), f.internal, "", nil, 200)
	entries, ok := page["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatal("ledger response lost opening")
	}
}
