package application

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/junglegaming/backend-challenge-go/internal/domain"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// Actor deve ser construído pelo adaptador após autenticar a identidade.
// Seus campos privados não pertencem ao corpo de uma requisição externa.
type Actor struct {
	providerID string
	internal   bool
}

func NewProviderActor(providerID string) (Actor, error) {
	if !validText(providerID) {
		return Actor{}, ErrInvalidInput
	}
	return Actor{providerID: providerID}, nil
}

func InternalActor() Actor { return Actor{internal: true} }

func (a Actor) authorizeInternal() error {
	if !a.internal && a.providerID == "" {
		return ErrUnauthenticated
	}
	if !a.internal {
		return ErrForbidden
	}
	return nil
}

func (a Actor) authorizeProvider(providerID string, allowInternal bool) error {
	if a.internal && allowInternal {
		return nil
	}
	if !a.internal && a.providerID == "" {
		return ErrUnauthenticated
	}
	if a.internal || a.providerID != providerID {
		return ErrForbidden
	}
	return nil
}

type RequestMetadata struct {
	CorrelationID string
	CausationID   string
}

type Observer interface {
	WagerResult(domain.TransactionStatus, bool)
	Reconciliation(bool)
	Conflict()
}

type Dependencies struct {
	Observer     Observer
	Transactions Transactor
	Now          func() time.Time
	NewID        func() (string, error)
	ReferenceTTL time.Duration
	Logger       *slog.Logger
}

type service struct{ dependencies Dependencies }

func newService(dependencies Dependencies) (service, error) {
	if dependencies.Transactions == nil || dependencies.ReferenceTTL < 0 {
		return service{}, ErrInvalidDependencies
	}
	if dependencies.Now == nil {
		dependencies.Now = time.Now
	}
	if dependencies.NewID == nil {
		dependencies.NewID = randomID
	}
	if dependencies.ReferenceTTL == 0 {
		dependencies.ReferenceTTL = 24 * time.Hour
	}
	if dependencies.Logger == nil {
		dependencies.Logger = slog.Default()
	}
	return service{dependencies: dependencies}, nil
}

func (s service) within(ctx context.Context, action func(Repositories) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := s.dependencies.Transactions.WithinTransaction(ctx, func(repositories Repositories) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := action(repositories); err != nil {
			return err
		}
		return ctx.Err()
	})
	if s.dependencies.Observer != nil && (errors.Is(err, ErrConflict) || errors.Is(err, ErrInboxConflict)) {
		s.dependencies.Observer.Conflict()
	}
	return err
}

func (s service) newID() (string, error) {
	id, err := s.dependencies.NewID()
	if err != nil {
		return "", fmt.Errorf("generate ID: %w", err)
	}
	if !validText(id) {
		return "", ErrInvalidDependencies
	}
	return id, nil
}

// PostgreSQL preserva microssegundos em TIMESTAMPTZ. Normalizar antes da
// criação mantém o mesmo instante na entidade, no resultado e no evento JSON.
func (s service) now() time.Time {
	return s.dependencies.Now().UTC().Truncate(time.Microsecond)
}

func validText(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && utf8.ValidString(value)
}

func validateMetadata(metadata RequestMetadata) error {
	if (metadata.CorrelationID != "" && !validText(metadata.CorrelationID)) ||
		(metadata.CausationID != "" && !validText(metadata.CausationID)) {
		return ErrInvalidInput
	}
	return nil
}

func randomID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[0:4], id[4:6], id[6:8], id[8:10], id[10:16]), nil
}

func (s service) observeResult(result ProcessWagerResult) {
	if s.dependencies.Observer != nil {
		s.dependencies.Observer.WagerResult(result.Transaction.Status, result.IdempotentReplay)
	}
}
