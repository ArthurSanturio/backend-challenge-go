// Package domain reúne os conceitos e as regras financeiras da aplicação.
package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Money representa dinheiro em unidades mínimas, com duas casas decimais.
// Seus campos são privados e suas operações retornam novos valores.
// O valor zero da struct é inválido; use NewMoney ou ZeroMoney.
type Money struct {
	minorUnits int64
	currency   string
}

// NewMoney valida uma entrada financeira externa no formato "25.00".
// Não aceita sinal, espaços, zeros à esquerda ou outras escalas.
func NewMoney(amount, currency string) (Money, error) {
	if err := validateCurrency(currency); err != nil {
		return Money{}, err
	}

	whole, fraction, found := strings.Cut(amount, ".")
	if !found || len(whole) == 0 || len(fraction) != 2 ||
		(len(whole) > 1 && whole[0] == '0') || !onlyDigits(whole) || !onlyDigits(fraction) {
		return Money{}, ErrInvalidAmount
	}

	// A concatenação transforma "25.00" em "2500", sem ponto flutuante.
	minorUnits, err := strconv.ParseInt(whole+fraction, 10, 64)
	if err != nil {
		return Money{}, fmt.Errorf("parse money: %w", ErrMoneyOverflow)
	}
	return Money{minorUnits: minorUnits, currency: currency}, nil
}

// NewMoneyFromMinorUnits cria valores internos ou reidrata dados do banco.
// Aceita negativos para cálculos e diferenças; entradas externas usam NewMoney.
func NewMoneyFromMinorUnits(minorUnits int64, currency string) (Money, error) {
	if err := validateCurrency(currency); err != nil {
		return Money{}, err
	}
	return Money{minorUnits: minorUnits, currency: currency}, nil
}

// ZeroMoney cria zero válido para uma moeda suportada.
func ZeroMoney(currency string) (Money, error) {
	return NewMoneyFromMinorUnits(0, currency)
}

// Validate rejeita valores não inicializados ou inválidos.
func (m Money) Validate() error {
	if err := validateCurrency(m.currency); err != nil {
		return ErrInvalidMoney
	}
	return nil
}

// MinorUnits devolve a quantia exata em centavos para as moedas suportadas.
func (m Money) MinorUnits() (int64, error) {
	if err := m.Validate(); err != nil {
		return 0, err
	}
	return m.minorUnits, nil
}

// Currency devolve o código ISO 4217 da moeda.
func (m Money) Currency() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	return m.currency, nil
}

// Amount formata a quantia com duas casas, inclusive diferenças negativas.
func (m Money) Amount() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}

	// Formatar primeiro como string evita negar math.MinInt64, que transbordaria.
	digits := strconv.FormatInt(m.minorUnits, 10)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	for len(digits) < 3 {
		digits = "0" + digits
	}
	split := len(digits) - 2
	return sign + digits[:split] + "." + digits[split:], nil
}

// Add soma valores da mesma moeda sem modificar os operandos.
func (m Money) Add(other Money) (Money, error) {
	if err := m.validatePair(other); err != nil {
		return Money{}, err
	}
	if (other.minorUnits > 0 && m.minorUnits > math.MaxInt64-other.minorUnits) ||
		(other.minorUnits < 0 && m.minorUnits < math.MinInt64-other.minorUnits) {
		return Money{}, ErrMoneyOverflow
	}
	return Money{minorUnits: m.minorUnits + other.minorUnits, currency: m.currency}, nil
}

// Subtract calcula uma diferença exata, que pode ser negativa.
func (m Money) Subtract(other Money) (Money, error) {
	if err := m.validatePair(other); err != nil {
		return Money{}, err
	}
	// Verificar diretamente também cobre a subtração de math.MinInt64.
	if (other.minorUnits > 0 && m.minorUnits < math.MinInt64+other.minorUnits) ||
		(other.minorUnits < 0 && m.minorUnits > math.MaxInt64+other.minorUnits) {
		return Money{}, ErrMoneyOverflow
	}
	return Money{minorUnits: m.minorUnits - other.minorUnits, currency: m.currency}, nil
}

// Negate retorna o valor com sinal invertido.
func (m Money) Negate() (Money, error) {
	if err := m.Validate(); err != nil {
		return Money{}, err
	}
	if m.minorUnits == math.MinInt64 {
		return Money{}, ErrMoneyOverflow
	}
	return Money{minorUnits: -m.minorUnits, currency: m.currency}, nil
}

// Compare retorna -1, 0 ou 1 se m for menor, igual ou maior que other.
func (m Money) Compare(other Money) (int, error) {
	if err := m.validatePair(other); err != nil {
		return 0, err
	}
	switch {
	case m.minorUnits < other.minorUnits:
		return -1, nil
	case m.minorUnits > other.minorUnits:
		return 1, nil
	default:
		return 0, nil
	}
}

// MarshalJSON preserva o contrato com amount como string decimal.
// Também permite serializar diferenças internas negativas de reconciliação.
func (m Money) MarshalJSON() ([]byte, error) {
	amount, err := m.Amount()
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	}{Amount: amount, Currency: m.currency})
}

func (m Money) validatePair(other Money) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if err := other.Validate(); err != nil {
		return err
	}
	if m.currency != other.currency {
		return ErrCurrencyMismatch
	}
	return nil
}

func validateCurrency(currency string) error {
	// Lista explícita de códigos ISO 4217 suportados nesta etapa.
	switch currency {
	case "BRL", "USD", "EUR":
		return nil
	default:
		return ErrInvalidCurrency
	}
}

func onlyDigits(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return len(value) > 0
}
