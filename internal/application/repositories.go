package application

import (
	"context"
	"time"

	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

// Transactor confirma todos os writes do callback juntos, ou reverte todos.
// Os repositórios recebidos devem compartilhar a mesma transação e devolver
// entidades reidratadas independentes, sem expor objetos de um cache compartilhado.
type Transactor interface {
	WithinTransaction(context.Context, func(Repositories) error) error
}

type Repositories struct {
	Inbox   InboxRepository
	Wallets WalletRepository
	Wagers  WagerRepository
	Ledger  LedgerRepository
	Outbox  OutboxRepository
}

type WalletRepository interface {
	Get(context.Context, string) (*domain.Wallet, error)
	GetByPlayerCurrency(context.Context, string, string) (*domain.Wallet, error)
	// Lock mantém a carteira estável até o fim da transação. Deve ser usado
	// por todos os escritores e pela reconciliação para garantir sua visão consistente.
	Lock(context.Context, string) (*domain.Wallet, error)
	Insert(context.Context, *domain.Wallet) error
	Update(context.Context, *domain.Wallet) error
}

type WagerRepository interface {
	Get(context.Context, string) (*domain.WagerTransaction, error)
	GetByExternalID(context.Context, string, string) (*domain.WagerTransaction, error)
	GetByIdempotencyKey(context.Context, string, string) (*domain.WagerTransaction, error)
	HasProcessedReversal(context.Context, string) (bool, error)
	Insert(context.Context, *domain.WagerTransaction) error
	Update(context.Context, *domain.WagerTransaction) error
}

// LedgerPosition define a ordenação estável por (createdAt, id).
type LedgerPosition struct {
	CreatedAt time.Time
	ID        string
}

type LedgerRepository interface {
	Insert(context.Context, domain.WalletLedgerEntry) error
	List(context.Context, string, *LedgerPosition, int) ([]domain.WalletLedgerEntry, error)
	// CalculateBalance soma créditos menos débitos, incluindo OPENING, com
	// precisão exata mesmo se os totais brutos excederem int64.
	CalculateBalance(context.Context, string, string) (domain.Money, int64, error)
}

// OutboxRecord contém um snapshot JSON imutável em uma string.
// Este contrato registra o evento; a publicação será implementada posteriormente.
type OutboxRecord struct {
	EventID     string
	AggregateID string
	EventType   string
	Version     int
	OccurredAt  time.Time
	Payload     string
}

type OutboxRepository interface {
	Insert(context.Context, OutboxRecord) error
}

// Claim serializa reentregas pela identidade da mensagem, dentro da mesma SQL tx.
type InboxRepository interface {
	Claim(context.Context, InboxRecord) (InboxRecord, bool, error)
	Complete(context.Context, string, string, string, time.Time) error
}
type InboxRecord struct {
	ConsumerName, MessageID, ProviderID, PayloadHash string
	ReceivedAt                                       time.Time
	TransactionID                                    string
	CompletedAt                                      time.Time
}
