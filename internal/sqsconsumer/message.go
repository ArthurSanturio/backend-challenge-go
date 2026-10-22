package sqsconsumer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

type envelope struct {
	MessageID     string    `json:"messageId"`
	Type          string    `json:"type"`
	OccurredAt    time.Time `json:"occurredAt"`
	CorrelationID string    `json:"correlationId,omitempty"`
	CausationID   string    `json:"causationId,omitempty"`
	Data          struct {
		ProviderID            string                 `json:"providerId"`
		ExternalTransactionID string                 `json:"externalTransactionId"`
		IdempotencyKey        string                 `json:"idempotencyKey"`
		PlayerID              string                 `json:"playerId"`
		WalletID              string                 `json:"walletId"`
		RoundID               string                 `json:"roundId"`
		GameID                string                 `json:"gameId"`
		Kind                  domain.TransactionKind `json:"kind"`
		Money                 struct {
			Amount   string `json:"amount"`
			Currency string `json:"currency"`
		} `json:"money"`
		ReferenceExternalTransactionID string `json:"referenceExternalTransactionId,omitempty"`
	} `json:"data"`
}

func decode(body string) (envelope, application.ProcessWagerInput, error) {
	var event envelope
	if len(body) > 1<<20 || !utf8.ValidString(body) || !strings.HasPrefix(strings.TrimSpace(body), "{") {
		return event, application.ProcessWagerInput{}, application.ErrInvalidInput
	}
	scan := json.NewDecoder(strings.NewReader(body))
	scan.UseNumber()
	if err := scanJSON(scan, 0); err != nil {
		return event, application.ProcessWagerInput{}, application.ErrInvalidInput
	}
	if _, err := scan.Token(); err != io.EOF {
		return event, application.ProcessWagerInput{}, application.ErrInvalidInput
	}
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil || event.Type != "WagerTransactionRequested" || event.OccurredAt.IsZero() || !validText(event.MessageID) {
		return event, application.ProcessWagerInput{}, application.ErrInvalidInput
	}
	money, err := domain.NewMoney(event.Data.Money.Amount, event.Data.Money.Currency)
	if err != nil {
		return event, application.ProcessWagerInput{}, application.ErrInvalidInput
	}
	switch event.Data.Kind {
	case domain.KindBet, domain.KindWin, domain.KindLoss, domain.KindRefund, domain.KindRollback:
	default:
		return event, application.ProcessWagerInput{}, application.ErrInvalidInput
	}
	if event.CorrelationID == "" {
		event.CorrelationID = event.MessageID
	}
	if event.CausationID == "" {
		event.CausationID = event.MessageID
	}
	return event, application.ProcessWagerInput{
		ProviderID: event.Data.ProviderID, ExternalTransactionID: event.Data.ExternalTransactionID, IdempotencyKey: event.Data.IdempotencyKey,
		PlayerID: event.Data.PlayerID, WalletID: event.Data.WalletID, RoundID: event.Data.RoundID, GameID: event.Data.GameID,
		Kind: event.Data.Kind, Money: money, ReferenceExternalTransactionID: event.Data.ReferenceExternalTransactionID,
		Metadata: application.RequestMetadata{CorrelationID: event.CorrelationID, CausationID: event.CausationID},
	}, nil
}
func validText(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && utf8.ValidString(value)
}

// IDs curtos e determinísticos, sem ambiguidade entre os componentes.
func MessageGroupID(providerID, walletID string) string {
	encoded, _ := json.Marshal([]string{providerID, walletID})
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}
func DeduplicationID(messageID string) string {
	hash := sha256.Sum256([]byte(messageID))
	return hex.EncodeToString(hash[:])
}
func scanJSON(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return application.ErrInvalidInput
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok || keys[key] {
				return application.ErrInvalidInput
			}
			keys[key] = true
			if err := scanJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return application.ErrInvalidInput
	}
	_, err = decoder.Token()
	return err
}
