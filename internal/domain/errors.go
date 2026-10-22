package domain

import "errors"

var (
	ErrInvalidMoney     = errors.New("money is not initialized or is invalid")
	ErrInvalidAmount    = errors.New("amount must be a non-negative decimal with exactly two decimal places")
	ErrInvalidCurrency  = errors.New("currency is not supported")
	ErrCurrencyMismatch = errors.New("money currencies do not match")
	ErrMoneyOverflow    = errors.New("money exceeds int64 limits")

	ErrInvalidWallet         = errors.New("wallet is not initialized or has invalid state")
	ErrNegativeBalance       = errors.New("wallet balance cannot be negative")
	ErrInvalidMovement       = errors.New("wallet movement must be greater than zero")
	ErrInsufficientFunds     = errors.New("wallet has insufficient funds")
	ErrInvalidWalletTime     = errors.New("wallet timestamp is zero or out of order")
	ErrWalletVersionOverflow = errors.New("wallet version exceeds int64 limits")

	ErrInvalidTransaction           = errors.New("transaction is not initialized or has invalid state")
	ErrInvalidTransactionAmount     = errors.New("amount is not allowed for this transaction kind")
	ErrInvalidTransactionTime       = errors.New("transaction timestamp is zero or out of order")
	ErrInvalidTransactionTransition = errors.New("transaction transition is not allowed")
	ErrInvalidReference             = errors.New("transaction reference is missing or not allowed")
	ErrReferenceNotProcessed        = errors.New("referenced transaction is not processed")
	ErrReferenceMismatch            = errors.New("transaction and reference do not match")
	ErrInvalidFailureCode           = errors.New("failure code is not allowed for this transaction")
	ErrInvalidLedgerEntry           = errors.New("ledger entry is not initialized or has invalid state")
	ErrInvalidLedgerTime            = errors.New("ledger timestamp is zero or precedes transaction processing")
	ErrNoLedgerMovement             = errors.New("transaction does not produce a ledger movement")
)
