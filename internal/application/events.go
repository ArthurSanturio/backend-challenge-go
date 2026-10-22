package application

import (
	"context"
	"encoding/json"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

type TransactionEventData struct {
	TransactionID                  string                 `json:"transactionId"`
	WalletID                       string                 `json:"walletId"`
	PlayerID                       string                 `json:"playerId"`
	ProviderID                     string                 `json:"providerId,omitempty"`
	ExternalTransactionID          string                 `json:"externalTransactionId,omitempty"`
	RoundID                        string                 `json:"roundId,omitempty"`
	GameID                         string                 `json:"gameId,omitempty"`
	Kind                           domain.TransactionKind `json:"kind"`
	Money                          domain.Money           `json:"money"`
	ReferenceExternalTransactionID string                 `json:"referenceExternalTransactionId,omitempty"`
}

type WagerTransactionProcessedData struct {
	TransactionEventData
	Balance domain.Money `json:"balance"`
}

type WagerTransactionRejectedData struct {
	TransactionEventData
	FailureCode domain.FailureCode `json:"failureCode"`
}

type WagerTransactionPendingReferenceData struct{ TransactionEventData }

type WalletBalanceChangedData struct {
	WalletID      string                 `json:"walletId"`
	TransactionID string                 `json:"transactionId"`
	Direction     domain.LedgerDirection `json:"direction"`
	Money         domain.Money           `json:"money"`
	BalanceBefore domain.Money           `json:"balanceBefore"`
	BalanceAfter  domain.Money           `json:"balanceAfter"`
	WalletVersion int64                  `json:"walletVersion"`
}

type eventEnvelope[T any] struct {
	EventID       string    `json:"eventId"`
	EventType     string    `json:"eventType"`
	AggregateID   string    `json:"aggregateId"`
	CorrelationID string    `json:"correlationId"`
	CausationID   string    `json:"causationId,omitempty"`
	OccurredAt    time.Time `json:"occurredAt"`
	Version       int       `json:"version"`
	Data          T         `json:"data"`
}

func (s service) appendTransactionEvents(ctx context.Context, repositories Repositories, transaction *domain.WagerTransaction, wallet *domain.Wallet, before domain.Money, metadata RequestMetadata) error {
	state, err := transaction.Snapshot()
	if err != nil {
		return err
	}
	if metadata.CorrelationID == "" {
		metadata.CorrelationID = state.ID
	}
	data := TransactionEventData{
		TransactionID: state.ID, WalletID: state.WalletID, PlayerID: state.PlayerID,
		ProviderID: state.ProviderID, ExternalTransactionID: state.ExternalTransactionID,
		RoundID: state.RoundID, GameID: state.GameID, Kind: state.Kind, Money: state.Money,
		ReferenceExternalTransactionID: state.ReferenceExternalTransactionID,
	}
	switch state.Status {
	case domain.StatusProcessed:
		err = appendEvent(ctx, s, repositories.Outbox, "WagerTransactionProcessed", state.ID, state.UpdatedAt, metadata,
			WagerTransactionProcessedData{TransactionEventData: data, Balance: state.ResultBalance})
	case domain.StatusRejected:
		err = appendEvent(ctx, s, repositories.Outbox, "WagerTransactionRejected", state.ID, state.UpdatedAt, metadata,
			WagerTransactionRejectedData{TransactionEventData: data, FailureCode: state.FailureCode})
	case domain.StatusPendingReference:
		err = appendEvent(ctx, s, repositories.Outbox, "WagerTransactionPendingReference", state.ID, state.UpdatedAt, metadata,
			WagerTransactionPendingReferenceData{TransactionEventData: data})
	default:
		return ErrInvalidInput
	}
	if err != nil {
		return err
	}
	if state.Status != domain.StatusProcessed || state.Direction == "" {
		return nil
	}
	walletState, err := wallet.Snapshot()
	if err != nil {
		return err
	}
	return appendEvent(ctx, s, repositories.Outbox, "WalletBalanceChanged", state.WalletID, state.UpdatedAt, metadata,
		WalletBalanceChangedData{
			WalletID: state.WalletID, TransactionID: state.ID, Direction: state.Direction,
			Money: state.Money, BalanceBefore: before, BalanceAfter: state.ResultBalance, WalletVersion: walletState.Version,
		})
}

func appendEvent[T any](ctx context.Context, s service, repository OutboxRepository, eventType, aggregateID string, occurredAt time.Time, metadata RequestMetadata, data T) error {
	id, err := s.newID()
	if err != nil {
		return err
	}
	envelope := eventEnvelope[T]{
		EventID: id, EventType: eventType, AggregateID: aggregateID,
		CorrelationID: metadata.CorrelationID, CausationID: metadata.CausationID,
		OccurredAt: occurredAt.UTC(), Version: 1, Data: data,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	return repository.Insert(ctx, OutboxRecord{
		EventID: id, AggregateID: aggregateID, EventType: eventType,
		Version: envelope.Version, OccurredAt: envelope.OccurredAt, Payload: string(payload),
	})
}
