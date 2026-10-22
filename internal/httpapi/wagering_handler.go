package httpapi

import (
	"net/http"

	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

func (a *api) processWager(w http.ResponseWriter, r *http.Request) {
	identity := identityOf(r)
	if identity.internal {
		writeError(w, application.ErrForbidden)
		return
	}
	key, err := singleHeader(r, "Idempotency-Key", true)
	if err != nil {
		writeError(w, err)
		return
	}
	var input processWagerRequest
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, err)
		return
	}
	if input.ProviderID != identity.providerID {
		writeError(w, application.ErrForbidden)
		return
	}
	// OPENING é uma operação interna, sem rota pública de envio.
	switch input.Kind {
	case domain.KindBet, domain.KindWin, domain.KindLoss, domain.KindRefund, domain.KindRollback:
	default:
		writeError(w, application.ErrInvalidInput)
		return
	}
	money, err := input.Money.money()
	if err != nil {
		writeError(w, err)
		return
	}
	result, err := a.config.Wagering.Process(r.Context(), identity.actor, application.ProcessWagerInput{
		ProviderID: identity.providerID, ExternalTransactionID: input.ExternalTransactionID, IdempotencyKey: key,
		WalletID: input.WalletID, PlayerID: input.PlayerID, RoundID: input.RoundID, GameID: input.GameID,
		Kind: input.Kind, Money: money, ReferenceExternalTransactionID: input.ReferenceExternalTransactionID, Metadata: metadataOf(r),
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	status := http.StatusOK
	switch result.Transaction.Status {
	case domain.StatusPending, domain.StatusPendingReference:
		status = http.StatusAccepted
	case domain.StatusRejected:
		status = http.StatusUnprocessableEntity
	case domain.StatusFailed:
		status = http.StatusUnprocessableEntity
	}
	a.config.Logger.InfoContext(r.Context(), "wager result", "correlationId", metadataOf(r).CorrelationID,
		"providerId", identity.providerID, "transactionId", result.Transaction.ID, "walletId", input.WalletID,
		"status", result.Transaction.Status, "idempotentReplay", result.IdempotentReplay)
	w.Header().Set("Location", "/wagering/transactions/"+result.Transaction.ID)
	writeJSON(w, status, processDTO(result))
}
func (a *api) getWager(w http.ResponseWriter, r *http.Request) {
	result, err := a.config.Wagering.Get(r.Context(), identityOf(r).actor, r.PathValue("transactionId"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, transactionDTO(result))
}
func (a *api) getWagerByExternalID(w http.ResponseWriter, r *http.Request) {
	result, err := a.config.Wagering.GetByExternalID(r.Context(), identityOf(r).actor, r.PathValue("providerId"), r.PathValue("externalTransactionId"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, transactionDTO(result))
}
