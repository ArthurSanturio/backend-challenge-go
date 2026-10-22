//go:build integration

package systemtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/junglegaming/backend-challenge-go/internal/brokerauth"
	"github.com/junglegaming/backend-challenge-go/internal/postgres"
	"github.com/junglegaming/backend-challenge-go/internal/sqsconsumer"
)

type object = map[string]any
type intercepted struct {
	index                  int
	eventID, receipt, body string
}
type faultGate struct {
	mode, wallet, external string
	claimed                atomic.Bool
	hit                    chan intercepted
}
type forwarder struct {
	index    int
	suite    *system
	server   *httptest.Server
	mu       sync.Mutex
	receipts map[string]string
}
type process struct {
	command      *exec.Cmd
	done         chan error
	address, log string
	stopped      bool
}
type system struct {
	referenceTTL                          string
	directory                             string
	allLogs                               []string
	t                                     *testing.T
	pool                                  *pgxpool.Pool
	databaseURL, issuer, binary, endpoint string
	internal, provider                    string
	client                                *http.Client
	broker                                *sqs.Client
	publisher                             *sqs.Client
	runtimeCredentials                    aws.Credentials
	queueURL, dlqURL, eventDLQ            string
	processes                             []*process
	proxies                               []*forwarder
	idpProxy                              *httptest.Server
	idpOffline, brokerOffline             atomic.Bool
	gateMu                                sync.RWMutex
	gate                                  *faultGate
	deliveriesMu                          sync.Mutex
	deliveries                            map[string][]intercepted
	acks                                  map[string][]int
}

func newSystem(t *testing.T) *system {
	t.Helper()
	source, issuer, endpoint := os.Getenv("POSTGRES_TEST_URL"), os.Getenv("OIDC_TEST_ISSUER_URL"), os.Getenv("SQS_TEST_ENDPOINT_URL")
	if source == "" || issuer == "" || endpoint == "" {
		t.Fatal("system integration requires POSTGRES_TEST_URL, OIDC_TEST_ISSUER_URL and SQS_TEST_ENDPOINT_URL; run scripts/test-system.sh")
	}
	s := &system{referenceTTL: "24h", directory: t.TempDir(), t: t, issuer: issuer, endpoint: endpoint, client: &http.Client{Timeout: 25 * time.Second}, deliveries: map[string][]intercepted{}, acks: map[string][]int{}}
	admin, err := postgres.Open(t.Context(), source)
	must(t, err)
	t.Cleanup(admin.Close)
	name := "system_test_" + strings.ToLower(rand.Text())
	_, err = admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	must(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		if err != nil {
			t.Error(err)
		}
	})
	parsed, err := url.Parse(source)
	must(t, err)
	parsed.Path, parsed.RawPath = "/"+name, ""
	s.databaseURL = parsed.String()
	s.pool, err = postgres.Open(t.Context(), s.databaseURL)
	must(t, err)
	t.Cleanup(s.pool.Close)
	var actual string
	must(t, s.pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&actual))
	if actual != name {
		t.Fatal("refusing to migrate a database not owned by this test")
	}
	migrations, err := filepath.Glob("../../migrations/*.up.sql")
	must(t, err)
	for _, file := range migrations {
		body, err := os.ReadFile(file)
		must(t, err)
		_, err = s.pool.Exec(t.Context(), string(body))
		must(t, err)
	}
	// A aplicação e a inbox usam SQL de produção; todas as migrations são aplicadas.
	s.broker, err = sqsconsumer.NewClient(t.Context(), "us-east-1", endpoint)
	must(t, err)
	must(t, sqsconsumer.Provision(t.Context(), s.broker))
	identities, err := brokerauth.SetupLocal(t.Context(), s.broker, t.TempDir())
	must(t, err)
	s.runtimeCredentials = identities.Runtime.Credentials
	s.publisher = brokerAs(s.broker, identities.Publisher.Credentials)
	for name, destination := range map[string]*string{sqsconsumer.QueueName: &s.queueURL, sqsconsumer.DLQName: &s.dlqURL, sqsconsumer.EventDLQName: &s.eventDLQ} {
		q, err := s.broker.GetQueueUrl(t.Context(), &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
		must(t, err)
		*destination = aws.ToString(q.QueueUrl)
	}
	s.binary = os.Getenv("API_TEST_BINARY")
	if s.binary == "" {
		s.binary = filepath.Join(t.TempDir(), "api")
		command := exec.CommandContext(t.Context(), "go", "build", "-race", "-o", s.binary, "./cmd/api")
		command.Dir = "../.."
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("build real API: %v\n%s", err, out)
		}
	}
	s.internal = s.token("wallet-service")
	s.provider = s.token("provider-a")
	issuerURL, err := url.Parse(issuer)
	must(t, err)
	s.idpProxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.idpOffline.Load() {
			http.Error(w, "injected IdP outage", 503)
			return
		}
		target := issuerURL.Scheme + "://" + issuerURL.Host + r.URL.Path
		request, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
		if err != nil {
			http.Error(w, "proxy", 502)
			return
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			http.Error(w, "proxy", 502)
			return
		}
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(s.idpProxy.Close)
	for i := 0; i < 3; i++ {
		proxy := &forwarder{index: i, suite: s, receipts: map[string]string{}}
		proxy.server = httptest.NewServer(http.HandlerFunc(proxy.handle))
		s.proxies = append(s.proxies, proxy)
		t.Cleanup(proxy.server.Close)
		s.processes = append(s.processes, nil)
	}
	t.Cleanup(func() {
		for i := range s.processes {
			s.stop(i, false)
		}
		for _, log := range s.allLogs {
			data, _ := os.ReadFile(log)
			if bytes.Contains(data, []byte("DATA RACE")) {
				t.Errorf("race detector found a race in %s", log)
			}
		}
	})
	for i := range s.processes {
		s.start(i)
	}
	pids := map[int]bool{}
	for _, p := range s.processes {
		pids[p.command.Process.Pid] = true
	}
	if len(pids) != 3 {
		t.Fatal("test requires three distinct OS processes")
	}
	t.Logf("three real API processes: %v", keys(pids))
	return s
}
func keys(m map[int]bool) []int {
	var result []int
	for key := range m {
		result = append(result, key)
	}
	return result
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func wait(t *testing.T, label string, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		select {
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for %s", label)
}
func (s *system) token(clientID string) string {
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientID + "-local-secret"}}
	response, err := s.client.PostForm(s.issuer+"/protocol/openid-connect/token", form)
	must(s.t, err)
	defer response.Body.Close()
	var token struct {
		AccessToken string `json:"access_token"`
	}
	must(s.t, json.NewDecoder(response.Body).Decode(&token))
	if response.StatusCode != 200 || token.AccessToken == "" {
		s.t.Fatal("real Keycloak did not issue token")
	}
	return token.AccessToken
}
func (s *system) start(index int) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(s.t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	must(s.t, listener.Close())
	file, err := os.CreateTemp(s.directory, fmt.Sprintf("process-%d-*.log", index))
	must(s.t, err)
	command := exec.Command(s.binary)
	environment := map[string]string{"DATABASE_URL": s.databaseURL, "HTTP_PORT": fmt.Sprint(port), "OIDC_ISSUER_URL": s.issuer, "OIDC_JWKS_URL": s.idpProxy.URL + "/realms/wallet/protocol/openid-connect/certs", "OIDC_AUDIENCE": "wallet-api", "SQS_ENABLED": "true", "WORKERS_ENABLED": "true", "AWS_REGION": "us-east-1", "AWS_ACCESS_KEY_ID": "test", "AWS_SECRET_ACCESS_KEY": "test", "SQS_ENDPOINT_URL": s.proxies[index].server.URL, "OUTBOX_MAX_ATTEMPTS": "2", "REFERENCE_MAX_ATTEMPTS": "3", "REFERENCE_TTL": s.referenceTTL, "GORACE": "halt_on_error=1"}
	environment["AWS_ACCESS_KEY_ID"], environment["AWS_SECRET_ACCESS_KEY"] = s.runtimeCredentials.AccessKeyID, s.runtimeCredentials.SecretAccessKey
	environment["AWS_SESSION_TOKEN"], environment["AWS_SHARED_CREDENTIALS_FILE"], environment["AWS_PROFILE"] = "", "", ""
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, override := environment[key]; !override {
			command.Env = append(command.Env, entry)
		}
	}
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	command.Stdout, command.Stderr = file, file
	must(s.t, command.Start())
	must(s.t, file.Close())
	p := &process{command: command, done: make(chan error, 1), address: fmt.Sprintf("http://127.0.0.1:%d", port), log: file.Name()}
	s.processes[index] = p
	s.allLogs = append(s.allLogs, p.log)
	go func() { p.done <- command.Wait() }()
	wait(s.t, "process readiness", 30*time.Second, func() bool {
		select {
		case err := <-p.done:
			data, _ := os.ReadFile(p.log)
			s.t.Fatalf("process startup failed: %v\n%s", err, data)
		default:
		}
		response, err := s.client.Get(p.address + "/health/ready")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == 200
	})
}
func (s *system) stop(index int, kill bool) {
	p := s.processes[index]
	if p == nil || p.stopped {
		return
	}
	p.stopped = true
	if kill {
		_ = p.command.Process.Kill()
	} else {
		_ = p.command.Process.Signal(syscall.SIGTERM)
	}
	select {
	case err := <-p.done:
		if err != nil && !kill {
			data, _ := os.ReadFile(p.log)
			s.t.Errorf("graceful shutdown failed: %v\n%s", err, data)
		}
	case <-time.After(35 * time.Second):
		_ = p.command.Process.Kill()
		s.t.Error("shutdown deadline exceeded")
	}
}
func (s *system) request(index int, method, path, token, key string, payload any) (int, object, error) {
	var reader io.Reader
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(s.t.Context(), method, s.processes[index].address+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	if reader != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := s.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	var result object
	err = json.NewDecoder(response.Body).Decode(&result)
	return response.StatusCode, result, err
}
func (s *system) call(index int, method, path, token, key string, payload any, want int) object {
	s.t.Helper()
	status, result, err := s.request(index, method, path, token, key, payload)
	must(s.t, err)
	if status != want {
		s.t.Fatalf("%s %s status=%d want=%d body=%v", method, path, status, want, result)
	}
	return result
}
func (s *system) open(player, value string) string {
	result := s.call(0, "POST", "/wallets", s.internal, "", object{"playerId": player, "initialBalance": object{"amount": value, "currency": "BRL"}}, 201)
	return result["id"].(string)
}
func wager(wallet, player, external, kind, value string) object {
	return object{"providerId": "provider-a", "externalTransactionId": external, "walletId": wallet, "playerId": player, "roundId": "round-1", "gameId": "game-1", "kind": kind, "money": object{"amount": value, "currency": "BRL"}}
}
func (s *system) reconcile(wallet, value string, entries int) {
	result := s.call(0, "POST", "/wallets/"+wallet+"/reconciliation", s.internal, "", nil, 200)
	for _, field := range []string{"storedBalance", "calculatedBalance"} {
		money := result[field].(map[string]any)
		if money["amount"] != value || money["currency"] != "BRL" {
			s.t.Fatalf("%s=%v want %s", field, money, value)
		}
	}
	if result["consistent"] != true || result["checkedEntries"] != float64(entries) {
		s.t.Fatalf("ledger mismatch: %v", result)
	}
}
func (s *system) scalar(query string, args ...any) int64 {
	var value int64
	must(s.t, s.pool.QueryRow(s.t.Context(), query, args...).Scan(&value))
	return value
}
func (s *system) transaction(external, status string) {
	wait(s.t, external+" -> "+status, 30*time.Second, func() bool {
		return s.scalar("SELECT COUNT(*) FROM wager_transactions WHERE provider_id='provider-a' AND external_transaction_id=$1 AND status=$2", external, status) == 1
	})
}
func (s *system) envelope(messageID string, input object) string {
	data := object{}
	for key, value := range input {
		data[key] = value
	}
	data["idempotencyKey"] = input["externalTransactionId"]
	body, err := json.Marshal(object{"messageId": messageID, "type": "WagerTransactionRequested", "occurredAt": time.Now().UTC().Format(time.RFC3339Nano), "data": data})
	must(s.t, err)
	return string(body)
}
func (s *system) send(body, group string) {
	_, err := s.publisher.SendMessage(s.t.Context(), &sqs.SendMessageInput{QueueUrl: aws.String(s.queueURL), MessageBody: aws.String(body), MessageGroupId: aws.String(group), MessageDeduplicationId: aws.String(rand.Text())})
	must(s.t, err)
}
func (s *system) arm(mode, wallet, external string) *faultGate {
	g := &faultGate{mode: mode, wallet: wallet, external: external, hit: make(chan intercepted, 1)}
	s.gateMu.Lock()
	s.gate = g
	s.gateMu.Unlock()
	return g
}
func (s *system) clearGate() { s.gateMu.Lock(); s.gate = nil; s.gateMu.Unlock() }
func (s *system) await(g *faultGate) intercepted {
	timeout := 30 * time.Second
	if g.mode == "before_delete" {
		// Após reiniciar consumidores, uma resposta de ReceiveMessage pode se
		// perder com o cancelamento do long polling. A mensagem só volta após
		// a visibilidade real da fila; o teste não pode expirar antes disso.
		timeout += time.Duration(sqsconsumer.VisibilitySeconds) * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case hit := <-g.hit:
		return hit
	case <-timer.C:
		s.t.Fatalf("fault boundary %s was not reached within %s", g.mode, timeout)
		return intercepted{}
	}
}

// Os proxies mantêm PostgreSQL, IdP e SQS reais; apenas interrompem o transporte
// em fronteiras controladas. Não substituem a lógica do broker ou do banco.
func (p *forwarder) handle(w http.ResponseWriter, r *http.Request) {
	s := p.suite
	if s.brokerOffline.Load() {
		http.Error(w, "injected broker outage", 503)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var request struct {
		QueueURL      string `json:"QueueUrl"`
		MessageBody   string `json:"MessageBody"`
		ReceiptHandle string `json:"ReceiptHandle"`
	}
	_ = json.Unmarshal(raw, &request)
	operation := r.Header.Get("X-Amz-Target")
	payload := request.MessageBody
	if strings.HasSuffix(operation, ".DeleteMessage") {
		p.mu.Lock()
		payload = p.receipts[request.ReceiptHandle]
		p.mu.Unlock()
	}
	var event struct {
		EventID   string `json:"eventId"`
		MessageID string `json:"messageId"`
		Data      struct {
			WalletID   string `json:"walletId"`
			ExternalID string `json:"externalTransactionId"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(payload), &event)
	s.gateMu.RLock()
	g := s.gate
	s.gateMu.RUnlock()
	matches := g != nil && (g.wallet == "" || g.wallet == event.Data.WalletID) && (g.external == "" || g.external == event.Data.ExternalID)
	sending := strings.HasSuffix(operation, ".SendMessage") && !strings.Contains(request.QueueURL, "-dlq.fifo")
	deleting := strings.HasSuffix(operation, ".DeleteMessage")
	hit := intercepted{index: p.index, eventID: event.EventID, receipt: request.ReceiptHandle, body: payload}
	block := matches && ((g.mode == "before_send" && sending) || (g.mode == "before_delete" && deleting) || (g.mode == "after_send" && sending)) && g.claimed.CompareAndSwap(false, true)
	if matches && g.mode == "reject_send" && sending {
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"__type":"InternalError","message":"injected delivery error"}`)
		return
	}
	if block && g.mode != "after_send" {
		g.hit <- hit
		select {
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Second):
			http.Error(w, "fault boundary timeout", 503)
			return
		}
	}
	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, s.endpoint+r.URL.RequestURI(), bytes.NewReader(raw))
	if err != nil {
		http.Error(w, "proxy", 502)
		return
	}
	upstream.Header = r.Header.Clone()
	// Preserve the signed Host header: the broker verifies SigV4 credentials.
	upstream.Host = r.Host
	if sending {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	response, err := http.DefaultClient.Do(upstream)
	if err != nil {
		http.Error(w, "proxy", 502)
		return
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode == 200 && strings.HasSuffix(operation, ".ReceiveMessage") {
		var received struct {
			Messages []struct {
				ReceiptHandle string `json:"ReceiptHandle"`
				Body          string `json:"Body"`
			} `json:"Messages"`
		}
		_ = json.Unmarshal(body, &received)
		p.mu.Lock()
		for _, message := range received.Messages {
			p.receipts[message.ReceiptHandle] = message.Body
		}
		p.mu.Unlock()
	}
	if response.StatusCode == 200 && strings.HasSuffix(operation, ".SendMessage") && event.EventID != "" {
		s.deliveriesMu.Lock()
		s.deliveries[event.EventID] = append(s.deliveries[event.EventID], hit)
		s.deliveriesMu.Unlock()
	}
	if response.StatusCode == 200 && deleting && event.MessageID != "" {
		s.deliveriesMu.Lock()
		s.acks[event.MessageID] = append(s.acks[event.MessageID], p.index)
		s.deliveriesMu.Unlock()
	}
	if block && g.mode == "after_send" {
		g.hit <- hit
		select {
		case <-r.Context().Done():
			return
		case <-time.After(30 * time.Second):
			http.Error(w, "fault boundary timeout", 503)
			return
		}
	}
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(body)
}

func TestDistributedInstancesAndFailures(t *testing.T) {
	s := newSystem(t)
	run := func(name string, fn func(*testing.T)) {
		t.Run(name, func(t *testing.T) {
			previous := s.t
			s.t = t
			defer func() {
				if t.Failed() {
					for _, p := range s.processes {
						data, _ := os.ReadFile(p.log)
						lines := strings.Split(string(data), "\n")
						if len(lines) > 12 {
							lines = lines[len(lines)-12:]
						}
						t.Logf("process tail %s:\n%s", p.log, strings.Join(lines, "\n"))
					}
				}
				s.t = previous
			}()
			fn(t)
		})
	}
	var originalWallet string
	var originalBet object
	run("50_parallel_HTTP_requests_and_SQS_redeliveries", func(t *testing.T) {
		originalWallet = s.open("duplicates", "100.00")
		originalBet = wager(originalWallet, "duplicates", "same-bet", "BET", "25.00")
		body := s.envelope("same-message", originalBet)
		start := make(chan struct{})
		failures := make(chan error, 50)
		var group sync.WaitGroup
		for i := 0; i < 50; i++ {
			group.Add(1)
			go func(i int) {
				defer group.Done()
				<-start
				status, result, err := s.request(i%3, "POST", "/wagering/transactions", s.provider, "same-bet", originalBet)
				if err != nil {
					failures <- err
					return
				}
				if status != 200 || result["status"] != "PROCESSED" {
					failures <- fmt.Errorf("parallel response %d %v", status, result)
				}
			}(i)
		}
		close(start)
		for i := 0; i < 10; i++ {
			s.send(body, sqsconsumer.MessageGroupID("provider-a", originalWallet))
		}
		group.Wait()
		close(failures)
		for err := range failures {
			t.Error(err)
		}
		wait(t, "one completed inbox after repeated envelopes", 30*time.Second, func() bool {
			return s.scalar("SELECT COUNT(*) FROM inbox_messages WHERE message_id='same-message' AND completed_at IS NOT NULL") == 1
		})
		wait(t, "all redeliveries acknowledged", 30*time.Second, func() bool {
			s.deliveriesMu.Lock()
			defer s.deliveriesMu.Unlock()
			return len(s.acks["same-message"]) >= 10
		})
		if s.scalar("SELECT COUNT(*) FROM wager_transactions WHERE provider_id='provider-a' AND external_transaction_id='same-bet'") != 1 {
			t.Fatal("financial movement duplicated")
		}
		s.reconcile(originalWallet, "75.00", 2)
		if replays := s.metricTotal("wallet_wager_results_total", `replay="true"`); replays < 59 {
			t.Fatalf("observed replay metric=%v want at least 59", replays)
		}
	})
	run("two_bets_of_80_on_balance_100", func(t *testing.T) {
		wallet := s.open("contended", "100.00")
		statuses := make(chan int, 2)
		errors := make(chan error, 2)
		var group sync.WaitGroup
		for i := 0; i < 2; i++ {
			group.Add(1)
			go func(i int) {
				defer group.Done()
				id := fmt.Sprintf("contended-%d", i)
				status, _, err := s.request(i, "POST", "/wagering/transactions", s.provider, id, wager(wallet, "contended", id, "BET", "80.00"))
				if err != nil {
					errors <- err
				}
				statuses <- status
			}(i)
		}
		group.Wait()
		close(statuses)
		close(errors)
		for err := range errors {
			t.Error(err)
		}
		processed, rejected := 0, 0
		for status := range statuses {
			if status == 200 {
				processed++
			}
			if status == 422 {
				rejected++
			}
		}
		if processed != 1 || rejected != 1 {
			t.Fatalf("processed=%d rejected=%d", processed, rejected)
		}
		s.reconcile(wallet, "20.00", 2)
	})
	run("independent_wallets_and_competing_publishers", func(t *testing.T) {
		type walletEntry struct{ wallet, player string }
		var wallets []walletEntry
		for i := 0; i < 12; i++ {
			player := fmt.Sprintf("independent-%d", i)
			wallets = append(wallets, walletEntry{s.open(player, "10.00"), player})
		}
		var group sync.WaitGroup
		failures := make(chan error, len(wallets))
		for i, entry := range wallets {
			group.Add(1)
			go func(i int, entry walletEntry) {
				defer group.Done()
				id := "bet-" + entry.player
				status, result, err := s.request(i%3, "POST", "/wagering/transactions", s.provider, id, wager(entry.wallet, entry.player, id, "BET", "1.00"))
				if err != nil {
					failures <- err
				} else if status != 200 {
					failures <- fmt.Errorf("%d %v", status, result)
				}
			}(i, entry)
		}
		group.Wait()
		close(failures)
		for err := range failures {
			t.Error(err)
		}
		for _, entry := range wallets {
			s.reconcile(entry.wallet, "9.00", 2)
		}
		wait(t, "outbox delivery by competing publishers", 40*time.Second, func() bool {
			return s.scalar("SELECT COUNT(*) FROM outbox_events WHERE published_at IS NULL AND dead_lettered_at IS NULL") == 0
		})
		s.deliveriesMu.Lock()
		publishers := map[int]bool{}
		for _, deliveries := range s.deliveries {
			for _, delivery := range deliveries {
				publishers[delivery.index] = true
			}
		}
		s.deliveriesMu.Unlock()
		if len(publishers) < 2 {
			t.Fatalf("expected competing publishers, observed %v", keys(publishers))
		}
		t.Logf("observed publishers in process slots %v", keys(publishers))
	})
	run("restart_preserves_idempotency_and_pending_reference", func(t *testing.T) {
		wallet := s.open("reference", "100.00")
		refund := wager(wallet, "reference", "early-refund", "REFUND", "10.00")
		refund["referenceExternalTransactionId"] = "late-bet"
		s.call(0, "POST", "/wagering/transactions", s.provider, "early-refund", refund, 202)
		// Controlar a agenda evita consumir o orçamento enquanto os processos reiniciam.
		_, err := s.pool.Exec(t.Context(), `UPDATE reference_jobs SET next_attempt_at=clock_timestamp()+INTERVAL '1 hour' WHERE transaction_id=(SELECT id FROM wager_transactions WHERE external_transaction_id='early-refund')`)
		must(t, err)
		for i := 0; i < 3; i++ {
			s.stop(i, false)
		}
		for i := 0; i < 3; i++ {
			s.start(i)
		}
		replay := s.call(2, "POST", "/wagering/transactions", s.provider, "same-bet", originalBet, 200)
		if replay["idempotentReplay"] != true || replay["balance"].(map[string]any)["amount"] != "75.00" {
			t.Fatal("idempotency did not survive restart")
		}
		s.call(1, "POST", "/wagering/transactions", s.provider, "late-bet", wager(wallet, "reference", "late-bet", "BET", "10.00"), 200)
		_, err = s.pool.Exec(t.Context(), `UPDATE reference_jobs SET next_attempt_at=clock_timestamp() WHERE transaction_id=(SELECT id FROM wager_transactions WHERE external_transaction_id='early-refund')`)
		must(t, err)
		s.transaction("early-refund", "PROCESSED")
		s.reconcile(wallet, "100.00", 3)
		another := wager(wallet, "reference", "duplicate-reversal", "ROLLBACK", "10.00")
		another["referenceExternalTransactionId"] = "late-bet"
		rejection := s.call(0, "POST", "/wagering/transactions", s.provider, "duplicate-reversal", another, 422)
		if rejection["failureCode"] != "ALREADY_REVERSED" {
			t.Fatalf("reversal rejection=%v", rejection)
		}
		s.reconcile(wallet, "100.00", 3)
	})
	run("reference_retry_limit_and_TTL", func(t *testing.T) {
		wallet := s.open("missing-reference", "50.00")
		input := wager(wallet, "missing-reference", "never-arrives", "REFUND", "5.00")
		input["referenceExternalTransactionId"] = "missing-never"
		s.call(0, "POST", "/wagering/transactions", s.provider, "never-arrives", input, 202)
		s.transaction("never-arrives", "REJECTED")
		for i := 0; i < 3; i++ {
			s.stop(i, false)
		}
		s.referenceTTL = "2s"
		for i := 0; i < 3; i++ {
			s.start(i)
		}
		input = wager(wallet, "missing-reference", "expired-reference", "REFUND", "5.00")
		input["referenceExternalTransactionId"] = "missing-expired"
		s.call(0, "POST", "/wagering/transactions", s.provider, "expired-reference", input, 202)
		s.transaction("expired-reference", "REJECTED")
		if s.scalar(`SELECT COUNT(*) FROM wager_transactions WHERE external_transaction_id IN ('never-arrives','expired-reference') AND failure_code='REFERENCE_NOT_FOUND'`) != 2 {
			t.Fatal("pending references not rejected durably")
		}
		for i := 0; i < 3; i++ {
			s.stop(i, false)
		}
		s.referenceTTL = "24h"
		for i := 0; i < 3; i++ {
			s.start(i)
		}
		s.reconcile(wallet, "50.00", 1)
	})
	run("crash_after_commit_before_SQS_delete", func(t *testing.T) {
		wallet := s.open("ack-crash", "50.00")
		input := wager(wallet, "ack-crash", "ack-crash-bet", "BET", "5.00")
		gate := s.arm("before_delete", "", "")
		defer s.clearGate()
		s.send(s.envelope("ack-crash-message", input), sqsconsumer.MessageGroupID("provider-a", wallet))
		hit := s.await(gate)
		if s.scalar(`SELECT COUNT(*) FROM inbox_messages WHERE message_id='ack-crash-message' AND completed_at IS NOT NULL`) != 1 {
			t.Fatal("delete attempted before inbox commit")
		}
		s.stop(hit.index, true)
		defer func() {
			if s.processes[hit.index].stopped {
				s.start(hit.index)
			}
		}()
		wait(t, "redelivery removed from real broker by a surviving process", 80*time.Second, func() bool {
			output, err := s.broker.GetQueueAttributes(t.Context(), &sqs.GetQueueAttributesInput{QueueUrl: aws.String(s.queueURL), AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages, types.QueueAttributeNameApproximateNumberOfMessagesNotVisible}})
			if err != nil {
				return false
			}
			return output.Attributes["ApproximateNumberOfMessages"] == "0" && output.Attributes["ApproximateNumberOfMessagesNotVisible"] == "0"
		})
		if s.scalar(`SELECT COUNT(*) FROM inbox_messages WHERE message_id='ack-crash-message' AND completed_at IS NOT NULL`) != 1 {
			t.Fatal("redelivery duplicated or lost durable inbox")
		}

		if hit.index == 0 {
			s.start(0)
		}
		s.reconcile(wallet, "45.00", 2)
		if hit.index != 0 {
			s.start(hit.index)
		}
		if s.scalar(`SELECT COUNT(*) FROM wager_transactions WHERE external_transaction_id='ack-crash-bet'`) != 1 {
			t.Fatal("redelivery duplicated debit")
		}
	})
	for _, mode := range []string{"before_send", "after_send"} {
		mode := mode
		run("publisher_crash_"+mode, func(t *testing.T) {
			player := "publisher-" + mode
			wallet := s.open(player, "20.00")
			external := player + "-win"
			gate := s.arm(mode, wallet, external)
			defer s.clearGate()
			s.call(0, "POST", "/wagering/transactions", s.provider, external, wager(wallet, player, external, "WIN", "1.00"), 200)
			hit := s.await(gate)
			s.stop(hit.index, true)
			if s.scalar(`SELECT COUNT(*) FROM outbox_events WHERE event_id=$1 AND published_at IS NULL`, hit.eventID) != 1 {
				t.Fatal("event acknowledged before delivery boundary")
			}
			// Avança somente o prazo da reserva do processo morto, sem esperar 60 segundos.
			_, err := s.pool.Exec(t.Context(), `UPDATE outbox_events SET lease_until=clock_timestamp()-INTERVAL '1 second' WHERE event_id=$1`, hit.eventID)
			must(t, err)
			wait(t, "another publisher confirms abandoned event", 30*time.Second, func() bool {
				return s.scalar(`SELECT COUNT(*) FROM outbox_events WHERE event_id=$1 AND published_at IS NOT NULL`, hit.eventID) == 1
			})
			s.deliveriesMu.Lock()
			deliveries := append([]intercepted(nil), s.deliveries[hit.eventID]...)
			s.deliveriesMu.Unlock()
			expected := 1
			if mode == "after_send" {
				expected = 2
			}
			if len(deliveries) < expected {
				t.Fatalf("observed deliveries=%d want >=%d", len(deliveries), expected)
			}
			for _, delivery := range deliveries {
				if delivery.body != hit.body || delivery.eventID != hit.eventID {
					t.Fatal("event identity or immutable payload changed on retry")
				}
			}
			s.start(hit.index)
			s.reconcile(wallet, "21.00", 2)
		})
	}
	run("publisher_retry_exhaustion_to_real_DLQ", func(t *testing.T) {
		wallet := s.open("publisher-dlq", "20.00")
		external := "dlq-win"
		s.arm("reject_send", wallet, external)
		defer s.clearGate()
		s.call(0, "POST", "/wagering/transactions", s.provider, external, wager(wallet, "publisher-dlq", external, "WIN", "1.00"), 200)
		var eventID string
		must(t, s.pool.QueryRow(t.Context(), `SELECT event_id FROM outbox_events WHERE transaction_id=(SELECT id FROM wager_transactions WHERE external_transaction_id=$1) AND event_type='WagerTransactionProcessed'`, external).Scan(&eventID))
		wait(t, "outbox DLQ confirmation", 40*time.Second, func() bool {
			return s.scalar(`SELECT COUNT(*) FROM outbox_events WHERE event_id=$1 AND dead_lettered_at IS NOT NULL AND published_at IS NULL`, eventID) == 1
		})
		message := s.receive(s.eventDLQ, func(body string) bool { return strings.Contains(body, eventID) })
		var payload string
		must(t, s.pool.QueryRow(t.Context(), `SELECT payload FROM outbox_events WHERE event_id=$1`, eventID).Scan(&payload))
		if aws.ToString(message.Body) != payload {
			t.Fatal("DLQ changed event snapshot")
		}
		s.reconcile(wallet, "21.00", 2)
	})
	run("invalid_input_redriven_by_real_broker", func(t *testing.T) {
		before := s.scalar("SELECT COUNT(*) FROM wager_transactions")
		s.send("{invalid-json", rand.Text())
		message := s.receive(s.dlqURL, func(body string) bool { return body == "{invalid-json" })
		if aws.ToString(message.Body) != "{invalid-json" || s.scalar("SELECT COUNT(*) FROM wager_transactions") != before {
			t.Fatal("invalid message altered financial state")
		}
	})
	run("readiness_and_recovery_for_broker_and_IdP", func(t *testing.T) {
		s.brokerOffline.Store(true)
		wait(t, "broker outage readiness", 15*time.Second, func() bool {
			status, _, err := s.request(0, "GET", "/health/ready", "", "", nil)
			return err == nil && status == 503
		})
		s.call(0, "GET", "/health/live", "", "", nil, 200)
		s.brokerOffline.Store(false)
		wait(t, "broker recovery", 30*time.Second, func() bool {
			status, _, err := s.request(0, "GET", "/health/ready", "", "", nil)
			return err == nil && status == 200
		})
		s.idpOffline.Store(true)
		result := s.call(0, "GET", "/health/ready", "", "", nil, 503)
		if result["checks"].(map[string]any)["idp"] != "unavailable" {
			t.Fatal("IdP outage not identified")
		}
		s.idpOffline.Store(false)
		wait(t, "IdP recovery", 15*time.Second, func() bool {
			status, _, err := s.request(0, "GET", "/health/ready", "", "", nil)
			return err == nil && status == 200
		})
	})
	run("PostgreSQL_outage_and_recovery", func(t *testing.T) {
		container := os.Getenv("POSTGRES_TEST_CONTAINER")
		if container == "" {
			t.Skip("POSTGRES_TEST_CONTAINER is required for the controlled Docker pause")
		}
		must(t, exec.Command("docker", "pause", container).Run())
		defer func() { _ = exec.Command("docker", "unpause", container).Run() }()
		result := s.call(0, "GET", "/health/ready", "", "", nil, 503)
		if result["checks"].(map[string]any)["postgresql"] != "unavailable" {
			t.Fatal("database outage not identified")
		}
		s.call(0, "GET", "/health/live", "", "", nil, 200)
		must(t, exec.Command("docker", "unpause", container).Run())
		wait(t, "database and worker recovery", 30*time.Second, func() bool {
			status, _, err := s.request(0, "GET", "/health/ready", "", "", nil)
			return err == nil && status == 200
		})
		s.reconcile(originalWallet, "75.00", 2)
	})
	run("missing_schema_readiness_and_recovery", func(t *testing.T) {
		_, err := s.pool.Exec(t.Context(), "ALTER TABLE reference_jobs RENAME TO hidden_reference_jobs")
		must(t, err)
		restored := false
		defer func() {
			if !restored {
				_, _ = s.pool.Exec(context.Background(), "ALTER TABLE hidden_reference_jobs RENAME TO reference_jobs")
			}
		}()
		result := s.call(0, "GET", "/health/ready", "", "", nil, 503)
		if result["checks"].(map[string]any)["schema"] != "unavailable" {
			t.Fatal("missing schema not identified")
		}
		_, err = s.pool.Exec(t.Context(), "ALTER TABLE hidden_reference_jobs RENAME TO reference_jobs")
		must(t, err)
		restored = true
		wait(t, "schema and worker recovery", 15*time.Second, func() bool {
			status, _, err := s.request(0, "GET", "/health/ready", "", "", nil)
			return err == nil && status == 200
		})
	})
	run("reconciliation_divergence_and_metrics", func(t *testing.T) {
		wallet := s.open("diagnostic", "10.00")
		// Somente no banco descartável: simula corrupção por administrador, que pode
		// desativar triggers. A API deve detectar e não reparar silenciosamente.
		corrupt := func(units int) {
			tx, err := s.pool.Begin(t.Context())
			must(t, err)
			defer tx.Rollback(t.Context())
			_, err = tx.Exec(t.Context(), "SET LOCAL session_replication_role=replica")
			must(t, err)
			_, err = tx.Exec(t.Context(), `UPDATE wallets SET balance_minor=$2 WHERE id=$1`, wallet, units)
			must(t, err)
			must(t, tx.Commit(t.Context()))
		}
		corrupt(1001)
		defer corrupt(1000)
		result := s.call(0, "POST", "/wallets/"+wallet+"/reconciliation", s.internal, "", nil, 200)
		if result["consistent"] != false || result["difference"].(map[string]any)["amount"] != "0.01" {
			t.Fatalf("divergence not reported: %v", result)
		}
		if s.scalar("SELECT balance_minor FROM wallets WHERE id=$1", wallet) != 1001 {
			t.Fatal("reconciliation altered stored balance")
		}
		s.call(0, "POST", "/wagering/transactions", s.provider, "same-bet", wager(originalWallet, "duplicates", "same-bet", "BET", "26.00"), 409)
		response, err := s.client.Get(s.processes[0].address + "/metrics")
		must(t, err)
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		must(t, err)
		for _, sample := range []string{`wallet_reconciliations_total{consistent="false"} 1`, `wallet_storage_metrics_up 1`, `wallet_http_requests_total`, `wallet_outbox_pending`, `wallet_outbox_oldest_age_seconds`, `wallet_sqs_dlq_depth`} {
			if !bytes.Contains(body, []byte(sample)) {
				t.Errorf("missing metric %s", sample)
			}
		}
	})
	run("Fx_partial_startup_failure_releases_resources", func(t *testing.T) {
		listener, err := net.Listen("tcp", ":0")
		must(t, err)
		defer listener.Close()
		port := listener.Addr().(*net.TCPAddr).Port
		startup, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		command := exec.CommandContext(startup, s.binary)
		command.WaitDelay = 3 * time.Second
		command.Env = append(append([]string{}, s.processes[0].command.Env...), fmt.Sprintf("HTTP_PORT=%d", port))
		output, err := command.CombinedOutput()
		if err == nil {
			t.Fatal("startup unexpectedly accepted an occupied HTTP port")
		}
		if !bytes.Contains(output, []byte("HTTP listener could not bind")) || !bytes.Contains(output, []byte("PostgreSQL connection closed")) {
			t.Fatalf("startup rollback not observed: %s", output)
		}
		s.call(0, "GET", "/health/ready", "", "", nil, 200)
	})
	run("all_remaining_balances_match_the_immutable_ledger", func(t *testing.T) {
		if s.scalar(`SELECT COUNT(*) FROM wallets w WHERE w.balance_minor::numeric != COALESCE((SELECT SUM(CASE WHEN direction='CREDIT' THEN amount_minor::numeric ELSE -amount_minor::numeric END) FROM wallet_ledger_entries l WHERE l.wallet_id=w.id),0)`) != 0 {
			t.Fatal("stored balances differ from credit-minus-debit ledger")
		}
		if s.scalar(`SELECT COUNT(*) FROM (SELECT wallet_id,transaction_id,COUNT(*) FROM wallet_ledger_entries GROUP BY wallet_id,transaction_id HAVING COUNT(*)>1) duplicates`) != 0 {
			t.Fatal("duplicate ledger movements")
		}
		t.Log("all wallet balances equal exact ledger totals; no duplicate financial entries")
	})
}

func (s *system) receive(queueURL string, match func(string) bool) types.Message {
	var found types.Message
	wait(s.t, "message in real dead-letter queue", 40*time.Second, func() bool {
		output, err := s.broker.ReceiveMessage(s.t.Context(), &sqs.ReceiveMessageInput{QueueUrl: aws.String(queueURL), MaxNumberOfMessages: 10, WaitTimeSeconds: 1, VisibilityTimeout: 5})
		if err != nil {
			return false
		}
		for _, message := range output.Messages {
			if match(aws.ToString(message.Body)) {
				found = message
				_, err := s.broker.DeleteMessage(s.t.Context(), &sqs.DeleteMessageInput{QueueUrl: aws.String(queueURL), ReceiptHandle: message.ReceiptHandle})
				must(s.t, err)
				return true
			}
		}
		return false
	})
	return found
}

func (s *system) metricTotal(name, label string) float64 {
	var total float64
	for _, process := range s.processes {
		response, err := s.client.Get(process.address + "/metrics")
		must(s.t, err)
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		must(s.t, err)
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, name+"{") && strings.Contains(line, label) {
				fields := strings.Fields(line)
				if len(fields) != 2 {
					s.t.Fatal("invalid metric sample")
				}
				value, err := strconv.ParseFloat(fields[1], 64)
				must(s.t, err)
				total += value
			}
		}
	}
	return total
}
