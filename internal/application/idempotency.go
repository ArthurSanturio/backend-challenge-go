package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func hashWager(input ProcessWagerInput) (string, error) {
	for _, value := range []string{
		input.ProviderID, input.ExternalTransactionID, input.IdempotencyKey,
		input.WalletID, input.PlayerID, input.RoundID, input.GameID,
	} {
		if !validText(value) {
			return "", ErrInvalidInput
		}
	}
	if input.ReferenceExternalTransactionID != "" && !validText(input.ReferenceExternalTransactionID) {
		return "", ErrInvalidInput
	}
	if err := validateMetadata(input.Metadata); err != nil {
		return "", err
	}
	amount, err := input.Money.Amount()
	if err != nil {
		return "", err
	}
	currency, err := input.Money.Currency()
	if err != nil {
		return "", err
	}
	// encoding/json ordena as chaves de maps de strings. Todos os valores de
	// negócio aqui são strings, incluindo dinheiro; não há ponto flutuante.
	payload := map[string]any{
		"providerId": input.ProviderID, "externalTransactionId": input.ExternalTransactionID,
		"walletId": input.WalletID, "playerId": input.PlayerID, "roundId": input.RoundID,
		"gameId": input.GameID, "kind": string(input.Kind),
		"money": map[string]string{"amount": amount, "currency": currency},
	}
	if input.ReferenceExternalTransactionID != "" {
		payload["referenceExternalTransactionId"] = input.ReferenceExternalTransactionID
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
