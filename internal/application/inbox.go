package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

type IncomingMessage struct {
	ConsumerName string
	MessageID    string
	OccurredAt   time.Time
}

// ProcessMessage usa o mesmo processamento financeiro de Process, adicionando
// inbox na mesma transação SQL. Nenhuma transação aninhada é aberta.
func (s *WageringService) ProcessMessage(ctx context.Context, actor Actor, message IncomingMessage, input ProcessWagerInput) (ProcessWagerResult, error) {
	if err := actor.authorizeProvider(input.ProviderID, false); err != nil {
		return ProcessWagerResult{}, err
	}
	if !validText(message.ConsumerName) || !validText(message.MessageID) || message.OccurredAt.IsZero() {
		return ProcessWagerResult{}, ErrInvalidInput
	}
	businessHash, err := hashWager(input)
	if err != nil {
		return ProcessWagerResult{}, err
	}
	// Inclui todos os campos aceitos do envelope, normalizando o instante para UTC.
	payload, err := json.Marshal(map[string]string{
		"type": "WagerTransactionRequested", "messageId": message.MessageID,
		"occurredAt":   message.OccurredAt.UTC().Format(time.RFC3339Nano),
		"businessHash": businessHash, "idempotencyKey": input.IdempotencyKey,
		"correlationId": input.Metadata.CorrelationID, "causationId": input.Metadata.CausationID,
	})
	if err != nil {
		return ProcessWagerResult{}, err
	}
	digest := sha256.Sum256(payload)
	hash := hex.EncodeToString(digest[:])
	var result ProcessWagerResult
	err = s.within(ctx, func(repositories Repositories) error {
		if repositories.Inbox == nil {
			return ErrInvalidDependencies
		}
		record, claimed, err := repositories.Inbox.Claim(ctx, InboxRecord{
			ConsumerName: message.ConsumerName, MessageID: message.MessageID, ProviderID: input.ProviderID,
			PayloadHash: hash, ReceivedAt: s.now(),
		})
		if err != nil {
			return err
		}
		if !claimed {
			if record.PayloadHash != hash || record.ProviderID != input.ProviderID {
				return ErrInboxConflict
			}
			if record.TransactionID == "" || record.CompletedAt.IsZero() {
				return ErrInvalidDependencies
			}
			transaction, err := repositories.Wagers.Get(ctx, record.TransactionID)
			if err != nil {
				return err
			}
			state, err := transaction.Snapshot()
			if err != nil {
				return err
			}
			if state.ProviderID != input.ProviderID {
				return ErrForbidden
			}
			result = ProcessWagerResult{Transaction: state, IdempotentReplay: true}
			return nil
		}
		result, err = s.process(ctx, repositories, input, businessHash)
		if err != nil {
			return err
		}
		return repositories.Inbox.Complete(ctx, message.ConsumerName, message.MessageID, result.Transaction.ID, s.now())
	})
	if err != nil {
		return ProcessWagerResult{}, err
	}
	s.observeResult(result)
	return result, nil
}
