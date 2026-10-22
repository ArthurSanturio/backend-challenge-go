package httpapi

import (
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/application"
	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

// Entradas usam strings: números JSON não são convertidos para dinheiro.
type moneyInput struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

func (m moneyInput) money() (domain.Money, error) { return domain.NewMoney(m.Amount, m.Currency) }

type createWalletRequest struct {
	PlayerID       string     `json:"playerId"`
	InitialBalance moneyInput `json:"initialBalance"`
}
type walletResponse struct {
	ID       string       `json:"id"`
	PlayerID string       `json:"playerId"`
	Balance  domain.Money `json:"balance"`
	Version  int64        `json:"version"`
}

func walletDTO(s domain.WalletState) walletResponse {
	return walletResponse{s.ID, s.PlayerID, s.Balance, s.Version}
}

type processWagerRequest struct {
	ProviderID                     string                 `json:"providerId"`
	ExternalTransactionID          string                 `json:"externalTransactionId"`
	PlayerID                       string                 `json:"playerId"`
	WalletID                       string                 `json:"walletId"`
	RoundID                        string                 `json:"roundId"`
	GameID                         string                 `json:"gameId"`
	Kind                           domain.TransactionKind `json:"kind"`
	Money                          moneyInput             `json:"money"`
	ReferenceExternalTransactionID string                 `json:"referenceExternalTransactionId"`
}
type processWagerResponse struct {
	TransactionID    string                   `json:"transactionId"`
	Status           domain.TransactionStatus `json:"status"`
	Balance          *domain.Money            `json:"balance,omitempty"`
	FailureCode      domain.FailureCode       `json:"failureCode,omitempty"`
	IdempotentReplay bool                     `json:"idempotentReplay"`
}

func processDTO(result application.ProcessWagerResult) processWagerResponse {
	s := result.Transaction
	return processWagerResponse{s.ID, s.Status, resultBalance(s), s.FailureCode, result.IdempotentReplay}
}
func resultBalance(s domain.WagerTransactionState) *domain.Money {
	if s.Status != domain.StatusProcessed {
		return nil
	}
	return &s.ResultBalance
}

type transactionResponse struct {
	TransactionID                  string                   `json:"transactionId"`
	ProviderID                     string                   `json:"providerId,omitempty"`
	ExternalTransactionID          string                   `json:"externalTransactionId,omitempty"`
	PlayerID                       string                   `json:"playerId"`
	WalletID                       string                   `json:"walletId"`
	RoundID                        string                   `json:"roundId,omitempty"`
	GameID                         string                   `json:"gameId,omitempty"`
	Kind                           domain.TransactionKind   `json:"kind"`
	Money                          domain.Money             `json:"money"`
	Status                         domain.TransactionStatus `json:"status"`
	Balance                        *domain.Money            `json:"balance,omitempty"`
	ReferenceExternalTransactionID string                   `json:"referenceExternalTransactionId,omitempty"`
	ReferenceTransactionID         string                   `json:"referenceTransactionId,omitempty"`
	FailureCode                    domain.FailureCode       `json:"failureCode,omitempty"`
	CreatedAt                      time.Time                `json:"createdAt"`
	UpdatedAt                      time.Time                `json:"updatedAt"`
}

func transactionDTO(s domain.WagerTransactionState) transactionResponse {
	return transactionResponse{
		TransactionID: s.ID, ProviderID: s.ProviderID, ExternalTransactionID: s.ExternalTransactionID,
		PlayerID: s.PlayerID, WalletID: s.WalletID, RoundID: s.RoundID, GameID: s.GameID, Kind: s.Kind,
		Money: s.Money, Status: s.Status, Balance: resultBalance(s), ReferenceExternalTransactionID: s.ReferenceExternalTransactionID,
		ReferenceTransactionID: s.ReferenceTransactionID, FailureCode: s.FailureCode, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
}

type ledgerEntryResponse struct {
	ID            string                 `json:"id"`
	WalletID      string                 `json:"walletId"`
	TransactionID string                 `json:"transactionId"`
	Direction     domain.LedgerDirection `json:"direction"`
	Money         domain.Money           `json:"money"`
	BalanceBefore domain.Money           `json:"balanceBefore"`
	BalanceAfter  domain.Money           `json:"balanceAfter"`
	CreatedAt     time.Time              `json:"createdAt"`
}
type ledgerPageResponse struct {
	Entries    []ledgerEntryResponse `json:"entries"`
	NextCursor string                `json:"nextCursor,omitempty"`
}

func ledgerDTO(page application.LedgerPage) ledgerPageResponse {
	result := ledgerPageResponse{Entries: make([]ledgerEntryResponse, 0, len(page.Entries)), NextCursor: page.NextCursor}
	for _, s := range page.Entries {
		result.Entries = append(result.Entries, ledgerEntryResponse{s.ID, s.WalletID, s.TransactionID, s.Direction, s.Money, s.BalanceBefore, s.BalanceAfter, s.CreatedAt})
	}
	return result
}

type reconciliationResponse struct {
	WalletID          string       `json:"walletId"`
	StoredBalance     domain.Money `json:"storedBalance"`
	CalculatedBalance domain.Money `json:"calculatedBalance"`
	Difference        domain.Money `json:"difference"`
	Consistent        bool         `json:"consistent"`
	CheckedEntries    int64        `json:"checkedEntries"`
}
