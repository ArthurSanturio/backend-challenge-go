package application_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

func TestWagerReplayKeepsOriginalBalanceAndEvents(t *testing.T) {
	f := newFixture(t)
	wallet := f.createWallet(t, 10000)
	input := wagerInput(t, wallet, domain.KindBet, 2500, "bet-1")
	input.Metadata = application.RequestMetadata{CorrelationID: "http-request", CausationID: "http-call"}
	first := f.process(t, input)
	if first.IdempotentReplay || first.Transaction.ResultBalance != units(t, 7500) || first.Transaction.IdempotencyKey != input.IdempotencyKey {
		t.Fatalf("unexpected first result: %+v", first)
	}
	canonical := `{"externalTransactionId":"bet-1","gameId":"game-1","kind":"BET","money":{"amount":"25.00","currency":"BRL"},"playerId":"player-1","providerId":"provider-a","roundId":"round-1","walletId":"id-0001"}`
	digest := sha256.Sum256([]byte(canonical))
	if first.Transaction.PayloadHash != hex.EncodeToString(digest[:]) {
		t.Fatalf("hash does not match canonical business JSON: %s", first.Transaction.PayloadHash)
	}
	f.process(t, wagerInput(t, wallet, domain.KindWin, 5000, "win-1"))
	before := f.store.data.clone()
	input.Metadata = application.RequestMetadata{CorrelationID: "sqs-request", CausationID: "message-1"}
	replay := f.process(t, input)
	if !replay.IdempotentReplay || replay.Transaction != first.Transaction || !reflect.DeepEqual(before, f.store.data) {
		t.Fatal("replay changed financial records, original result or events")
	}
	for _, change := range []func(*application.ProcessWagerInput){
		func(i *application.ProcessWagerInput) { i.Money = units(t, 2600) },
		func(i *application.ProcessWagerInput) { i.ExternalTransactionID = "another-external" },
		func(i *application.ProcessWagerInput) { i.IdempotencyKey = "another-key" },
		func(i *application.ProcessWagerInput) { i.RoundID = "another-round" },
	} {
		conflicting := input
		change(&conflicting)
		_, err := f.wagers.Process(t.Context(), f.actor, conflicting)
		if !errors.Is(err, application.ErrConflict) || !reflect.DeepEqual(before, f.store.data) {
			t.Fatalf("idempotency conflict changed records: %v", err)
		}
	}
}

func TestBetRejectionIsAuditableAndReplayable(t *testing.T) {
	f := newFixture(t)
	wallet := f.createWallet(t, 10000)
	first := f.process(t, wagerInput(t, wallet, domain.KindBet, 8000, "bet-1"))
	if first.Transaction.Status != domain.StatusProcessed {
		t.Fatal("first bet was not processed")
	}
	input := wagerInput(t, wallet, domain.KindBet, 8000, "bet-2")
	before := f.store.data.wallets[wallet.ID]
	ledgerCount, eventCount := len(f.store.data.ledger), len(f.store.data.outbox)
	second := f.process(t, input)
	if second.Transaction.Status != domain.StatusRejected || second.Transaction.FailureCode != domain.FailureInsufficientFunds ||
		f.store.data.wallets[wallet.ID] != before || before.Balance != units(t, 2000) || len(f.store.data.ledger) != ledgerCount {
		t.Fatalf("unexpected rejected bet: %+v", second)
	}
	if len(f.store.data.outbox) != eventCount+1 || f.store.data.outbox[eventCount].EventType != "WagerTransactionRejected" {
		t.Fatal("rejection did not record exactly its rejection event")
	}
	replay := f.process(t, input)
	if !replay.IdempotentReplay || replay.Transaction != second.Transaction || len(f.store.data.outbox) != eventCount+1 {
		t.Fatal("rejected replay changed the outcome")
	}
}

func TestReplayDoesNotNeedNewIDsOrClock(t *testing.T) {
	f := newFixture(t)
	wallet := f.createWallet(t, 10000)
	input := wagerInput(t, wallet, domain.KindBet, 2500, "bet-1")
	first := f.process(t, input)
	before := f.store.data.clone()
	f.idErr, f.now = errStorage, time.Time{}
	replay := f.process(t, input)
	if !replay.IdempotentReplay || replay.Transaction != first.Transaction || !reflect.DeepEqual(before, f.store.data) {
		t.Fatal("replay depended on generating new state")
	}
}

func TestLossOnlyCreatesProcessedTransactionEvent(t *testing.T) {
	f := newFixture(t)
	wallet := f.createWallet(t, 10000)
	before := f.store.data.wallets[wallet.ID]
	ledgerCount, eventCount := len(f.store.data.ledger), len(f.store.data.outbox)
	result := f.process(t, wagerInput(t, wallet, domain.KindLoss, 0, "loss-1"))
	if result.Transaction.Status != domain.StatusProcessed || result.Transaction.Direction != "" ||
		f.store.data.wallets[wallet.ID] != before || len(f.store.data.ledger) != ledgerCount || len(f.store.data.outbox) != eventCount+1 ||
		f.store.data.outbox[eventCount].EventType != "WagerTransactionProcessed" {
		t.Fatal("LOSS changed wallet, ledger or produced a balance event")
	}
}

func TestProcessedReversalsPreventDuplicateRefunds(t *testing.T) {
	for _, firstKind := range []domain.TransactionKind{domain.KindRefund, domain.KindRollback} {
		for _, secondKind := range []domain.TransactionKind{domain.KindRefund, domain.KindRollback} {
			t.Run(string(firstKind)+"/"+string(secondKind), func(t *testing.T) {
				f := newFixture(t)
				wallet := f.createWallet(t, 10000)
				f.process(t, wagerInput(t, wallet, domain.KindBet, 2500, "original-bet"))
				firstInput := wagerInput(t, wallet, firstKind, 2500, "reversal-1")
				firstInput.ReferenceExternalTransactionID = "original-bet"
				first := f.process(t, firstInput)
				if first.Transaction.Status != domain.StatusProcessed || f.store.data.wallets[wallet.ID].Balance != units(t, 10000) {
					t.Fatalf("first reversal failed: %+v", first)
				}
				before := f.store.data.wallets[wallet.ID]
				ledgerCount := len(f.store.data.ledger)
				secondInput := wagerInput(t, wallet, secondKind, 2500, "reversal-2")
				secondInput.ReferenceExternalTransactionID = "original-bet"
				second := f.process(t, secondInput)
				if second.Transaction.FailureCode != domain.FailureAlreadyReversed || second.Transaction.Status != domain.StatusRejected ||
					f.store.data.wallets[wallet.ID] != before || len(f.store.data.ledger) != ledgerCount {
					t.Fatalf("second reversal duplicated a refund: %+v", second)
				}
			})
		}
	}
}

func TestRollbackOfRefundDoesNotPermitAnotherDirectRefund(t *testing.T) {
	f := newFixture(t)
	wallet := f.createWallet(t, 10000)
	f.process(t, wagerInput(t, wallet, domain.KindBet, 2500, "bet-1"))
	refund := wagerInput(t, wallet, domain.KindRefund, 2500, "refund-1")
	refund.ReferenceExternalTransactionID = "bet-1"
	f.process(t, refund)
	rollback := wagerInput(t, wallet, domain.KindRollback, 2500, "rollback-refund")
	rollback.ReferenceExternalTransactionID = "refund-1"
	result := f.process(t, rollback)
	if result.Transaction.Status != domain.StatusProcessed || result.Transaction.Direction != domain.DirectionDebit ||
		result.Transaction.ResultBalance != units(t, 7500) {
		t.Fatalf("unexpected rollback of refund: %+v", result)
	}
	refund.ExternalTransactionID, refund.IdempotencyKey = "refund-2", "key-refund-2"
	if got := f.process(t, refund); got.Transaction.FailureCode != domain.FailureAlreadyReversed {
		t.Fatalf("refund history was reset by rollback: %+v", got)
	}
}

func TestRollbackInsufficientFundsHasDistinctCode(t *testing.T) {
	f := newFixture(t)
	wallet := f.createWallet(t, 0)
	f.process(t, wagerInput(t, wallet, domain.KindWin, 2500, "win-1"))
	f.process(t, wagerInput(t, wallet, domain.KindBet, 2500, "bet-1"))
	input := wagerInput(t, wallet, domain.KindRollback, 2500, "rollback-1")
	input.ReferenceExternalTransactionID = "win-1"
	before := f.store.data.wallets[wallet.ID]
	ledgerCount := len(f.store.data.ledger)
	result := f.process(t, input)
	if result.Transaction.FailureCode != domain.FailureReversalInsufficientFunds || result.Transaction.Status != domain.StatusRejected ||
		f.store.data.wallets[wallet.ID] != before || len(f.store.data.ledger) != ledgerCount {
		t.Fatalf("unexpected rollback rejection: %+v", result)
	}
	bet := f.process(t, wagerInput(t, wallet, domain.KindBet, 100, "bet-2"))
	if bet.Transaction.FailureCode == result.Transaction.FailureCode || bet.Transaction.FailureCode != domain.FailureInsufficientFunds {
		t.Fatal("BET and ROLLBACK use the same insufficient funds code")
	}
}

func TestMissingReferenceCanBeResumedWithoutDuplicateEvents(t *testing.T) {
	f := newFixture(t)
	wallet := f.createWallet(t, 10000)
	input := wagerInput(t, wallet, domain.KindRefund, 2500, "refund-1")
	input.ReferenceExternalTransactionID = "bet-later"
	before := f.store.data.wallets[wallet.ID]
	pending := f.process(t, input)
	if pending.Transaction.Status != domain.StatusPendingReference || f.store.data.wallets[wallet.ID] != before {
		t.Fatalf("unexpected pending reference: %+v", pending)
	}
	eventCount := len(f.store.data.outbox)
	if replay := f.process(t, input); !replay.IdempotentReplay || replay.Transaction != pending.Transaction {
		t.Fatal("pending replay changed transaction")
	}
	stillPending, err := f.wagers.Resume(t.Context(), application.InternalActor(), pending.Transaction.ID, application.RequestMetadata{})
	if err != nil || stillPending.Transaction != pending.Transaction || len(f.store.data.outbox) != eventCount {
		t.Fatalf("pending retry changed state or events: %+v, %v", stillPending, err)
	}
	f.process(t, wagerInput(t, wallet, domain.KindBet, 2500, "bet-later"))
	resolved, err := f.wagers.Resume(t.Context(), application.InternalActor(), pending.Transaction.ID, application.RequestMetadata{})
	if err != nil || resolved.Transaction.Status != domain.StatusProcessed || resolved.Transaction.ResultBalance != units(t, 10000) {
		t.Fatalf("reference was not resolved: %+v, %v", resolved, err)
	}
	stored := f.store.data.clone()
	terminal, err := f.wagers.Resume(t.Context(), application.InternalActor(), pending.Transaction.ID, application.RequestMetadata{})
	if err != nil || !terminal.IdempotentReplay || !reflect.DeepEqual(stored, f.store.data) {
		t.Fatal("resuming a terminal operation changed records")
	}
}

func TestReferenceExpirationAndUnsuccessfulOriginal(t *testing.T) {
	f := newFixture(t)
	wallet := f.createWallet(t, 10000)
	input := wagerInput(t, wallet, domain.KindRefund, 2500, "refund-1")
	input.ReferenceExternalTransactionID = "missing-bet"
	pending := f.process(t, input)
	f.now = f.now.Add(24 * time.Hour)
	expired, err := f.wagers.Resume(t.Context(), application.InternalActor(), pending.Transaction.ID, application.RequestMetadata{})
	if err != nil || expired.Transaction.Status != domain.StatusRejected || expired.Transaction.FailureCode != domain.FailureReferenceNotFound {
		t.Fatalf("unexpected expiration: %+v, %v", expired, err)
	}
	f.process(t, wagerInput(t, wallet, domain.KindBet, 20000, "rejected-bet"))
	input.ExternalTransactionID, input.IdempotencyKey, input.ReferenceExternalTransactionID = "refund-2", "key-refund-2", "rejected-bet"
	rejected := f.process(t, input)
	if rejected.Transaction.FailureCode != domain.FailureReferenceNotProcessed || rejected.Transaction.Status != domain.StatusRejected {
		t.Fatalf("unsuccessful reference was not rejected: %+v", rejected)
	}
}

func TestReferenceMismatchAndPartialRefundAreAudited(t *testing.T) {
	f := newFixture(t)
	wallet := f.createWallet(t, 10000)
	f.process(t, wagerInput(t, wallet, domain.KindBet, 2500, "bet-1"))
	for _, test := range []struct {
		externalID string
		amount     int64
		roundID    string
	}{
		{"partial-refund", 1000, "round-1"}, {"wrong-round", 2500, "other-round"},
	} {
		input := wagerInput(t, wallet, domain.KindRefund, test.amount, test.externalID)
		input.ReferenceExternalTransactionID, input.RoundID = "bet-1", test.roundID
		before := f.store.data.wallets[wallet.ID]
		result := f.process(t, input)
		if result.Transaction.FailureCode != domain.FailureReferenceMismatch || f.store.data.wallets[wallet.ID] != before {
			t.Fatalf("incompatible refund was applied: %+v", result)
		}
	}
}

func TestUseCasesIsolateProvidersIncludingReplaysAndQueries(t *testing.T) {
	f := newFixture(t)
	wallet := f.createWallet(t, 10000)
	input := wagerInput(t, wallet, domain.KindBet, 100, "bet-1")
	first := f.process(t, input)
	other, err := application.NewProviderActor("provider-b")
	if err != nil {
		t.Fatal(err)
	}
	before := f.store.data.clone()
	_, replayErr := f.wagers.Process(t.Context(), other, input)
	_, getErr := f.wagers.Get(t.Context(), other, first.Transaction.ID)
	_, externalErr := f.wagers.GetByExternalID(t.Context(), other, "provider-a", "bet-1")
	_, openingErr := f.wagers.Get(t.Context(), f.actor, "opening:"+wallet.ID)
	for _, err := range []error{replayErr, getErr, externalErr, openingErr} {
		if !errors.Is(err, application.ErrForbidden) {
			t.Errorf("error = %v, want ErrForbidden", err)
		}
	}
	if !reflect.DeepEqual(before, f.store.data) {
		t.Fatal("unauthorized calls changed financial records")
	}
	loaded, err := f.wagers.Get(t.Context(), f.actor, first.Transaction.ID)
	if err != nil || loaded != first.Transaction {
		t.Fatalf("authorized query failed: %+v, %v", loaded, err)
	}
	external, err := f.wagers.GetByExternalID(t.Context(), f.actor, "provider-a", "bet-1")
	if err != nil || external != first.Transaction {
		t.Fatalf("authorized external query failed: %+v, %v", external, err)
	}
	// A mesma chave literal em outro provedor pertence a outro escopo.
	input.ProviderID = "provider-b"
	second, err := f.wagers.Process(t.Context(), other, input)
	if err != nil || second.Transaction.ID == first.Transaction.ID || second.IdempotentReplay ||
		second.Transaction.PayloadHash == first.Transaction.PayloadHash {
		t.Fatalf("provider scopes collided: %+v, %v", second, err)
	}
}

func TestProcessFailureRollsBackWalletTransactionLedgerAndEvents(t *testing.T) {
	for _, step := range []string{"wager.key", "wallet.lock", "wallet.update", "wager.insert", "ledger.insert", "outbox.insert", "outbox.insert.2", "commit"} {
		t.Run(step, func(t *testing.T) {
			f := newFixture(t)
			wallet := f.createWallet(t, 10000)
			before := f.store.data.clone()
			input := wagerInput(t, wallet, domain.KindBet, 2500, "bet-1")
			f.store.failAt = step
			result, err := f.wagers.Process(t.Context(), f.actor, input)
			if !errors.Is(err, errStorage) || result != (application.ProcessWagerResult{}) || !reflect.DeepEqual(before, f.store.data) {
				t.Fatalf("partial processing after %s: %+v, %v", step, result, err)
			}
			f.store.failAt = ""
			retry := f.process(t, input)
			if retry.IdempotentReplay || retry.Transaction.ResultBalance != units(t, 7500) {
				t.Fatalf("retry after rollback failed: %+v", retry)
			}
		})
	}
}

func TestOutboxEnvelopeAndMoneyAreStableSnapshots(t *testing.T) {
	f := newFixture(t)
	wallet := f.createWallet(t, 10000)
	input := wagerInput(t, wallet, domain.KindBet, 2500, "bet-1")
	input.Metadata = application.RequestMetadata{CorrelationID: "correlation-1", CausationID: "message-1"}
	result := f.process(t, input)
	var before string
	for _, record := range f.store.data.outbox {
		var envelope struct {
			EventID       string          `json:"eventId"`
			EventType     string          `json:"eventType"`
			AggregateID   string          `json:"aggregateId"`
			CorrelationID string          `json:"correlationId"`
			CausationID   string          `json:"causationId"`
			OccurredAt    time.Time       `json:"occurredAt"`
			Version       int             `json:"version"`
			Data          json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal([]byte(record.Payload), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.EventID != record.EventID || envelope.EventType != record.EventType || envelope.AggregateID != record.AggregateID ||
			envelope.Version != 1 || envelope.OccurredAt != f.now || envelope.CorrelationID == "" {
			t.Fatalf("invalid event envelope: %+v", envelope)
		}
		if record.EventType == "WalletBalanceChanged" && envelope.CorrelationID == "correlation-1" {
			var data struct {
				TransactionID string `json:"transactionId"`
				WalletVersion int64  `json:"walletVersion"`
				Money         struct {
					Amount string `json:"amount"`
				} `json:"money"`
				BalanceAfter struct {
					Amount string `json:"amount"`
				} `json:"balanceAfter"`
			}
			if err := json.Unmarshal(envelope.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.TransactionID != result.Transaction.ID || data.WalletVersion != 2 || data.Money.Amount != "25.00" ||
				data.BalanceAfter.Amount != "75.00" || envelope.CausationID != "message-1" {
				t.Fatalf("invalid balance event: %+v", data)
			}
			before = record.Payload
		}
	}
	if before == "" {
		t.Fatal("balance event was not found")
	}
	f.process(t, wagerInput(t, wallet, domain.KindWin, 1000, "win-1"))
	found := false
	for _, record := range f.store.data.outbox {
		found = found || record.Payload == before
	}
	if !found {
		t.Fatal("later processing changed an event snapshot")
	}
}

func TestInvalidInputAndMissingIdentityHaveNoEffects(t *testing.T) {
	f := newFixture(t)
	wallet := f.createWallet(t, 10000)
	input := wagerInput(t, wallet, domain.KindBet, 100, "bet-1")
	before := f.store.data.clone()
	_, err := f.wagers.Process(t.Context(), application.Actor{}, input)
	if !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("missing actor: %v", err)
	}
	_, err = f.wallets.Get(t.Context(), application.Actor{}, wallet.ID)
	if !errors.Is(err, application.ErrUnauthenticated) {
		t.Fatalf("missing wallet actor: %v", err)
	}
	for _, change := range []func(*application.ProcessWagerInput){
		func(i *application.ProcessWagerInput) { i.IdempotencyKey = "" },
		func(i *application.ProcessWagerInput) { i.Kind = domain.KindOpening },
		func(i *application.ProcessWagerInput) { i.Money = units(t, -1) },
		func(i *application.ProcessWagerInput) { i.Money = domain.Money{} },
		func(i *application.ProcessWagerInput) { i.RoundID = "\xff" },
	} {
		invalid := input
		change(&invalid)
		if _, err := f.wagers.Process(t.Context(), f.actor, invalid); err == nil {
			t.Fatal("invalid financial request accepted")
		}
	}
	if !reflect.DeepEqual(before, f.store.data) {
		t.Fatal("invalid input had financial effects")
	}
}
