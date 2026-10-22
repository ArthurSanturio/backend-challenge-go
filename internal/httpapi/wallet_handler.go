package httpapi

import (
	"net/http"
	"strconv"

	"github.com/junglegaming/backend-challenge-go/internal/application"
)

func (a *api) createWallet(w http.ResponseWriter, r *http.Request) {
	var input createWalletRequest
	if err := readJSON(w, r, &input); err != nil {
		writeError(w, err)
		return
	}
	money, err := input.InitialBalance.money()
	if err != nil {
		writeError(w, err)
		return
	}
	result, err := a.config.Wallets.Create(r.Context(), identityOf(r).actor, application.CreateWalletInput{
		PlayerID: input.PlayerID, InitialBalance: money, Metadata: metadataOf(r),
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/wallets/"+result.ID)
	writeJSON(w, http.StatusCreated, walletDTO(result))
}
func (a *api) getWallet(w http.ResponseWriter, r *http.Request) {
	result, err := a.config.Wallets.Get(r.Context(), identityOf(r).actor, r.PathValue("walletId"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, walletDTO(result))
}
func (a *api) listLedger(w http.ResponseWriter, r *http.Request) {
	query, err := parseLedgerQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	result, err := a.config.Wallets.ListLedger(r.Context(), identityOf(r).actor, r.PathValue("walletId"), query.cursor, query.limit)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, ledgerDTO(result))
}

type ledgerQuery struct {
	cursor string
	limit  int
}

func parseLedgerQuery(r *http.Request) (ledgerQuery, error) {
	values, err := parseQuery(r)
	if err != nil {
		return ledgerQuery{}, err
	}
	for key, entries := range values {
		if (key != "cursor" && key != "limit") || len(entries) != 1 {
			return ledgerQuery{}, application.ErrInvalidInput
		}
	}
	result := ledgerQuery{cursor: values.Get("cursor"), limit: 50}
	if values.Has("limit") {
		result.limit, err = strconv.Atoi(values.Get("limit"))
		if err != nil || result.limit < 1 || result.limit > 200 {
			return ledgerQuery{}, application.ErrInvalidInput
		}
	}
	return result, nil
}
func (a *api) reconcile(w http.ResponseWriter, r *http.Request) {
	result, err := a.config.Wallets.Reconcile(r.Context(), identityOf(r).actor, r.PathValue("walletId"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, reconciliationResponse{
		WalletID: result.WalletID, StoredBalance: result.StoredBalance, CalculatedBalance: result.CalculatedBalance,
		Difference: result.Difference, Consistent: result.Consistent, CheckedEntries: result.CheckedEntries,
	})
}
