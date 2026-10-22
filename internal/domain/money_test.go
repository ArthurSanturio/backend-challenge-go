package domain_test

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"testing"

	"github.com/junglegaming/backend-challenge-go/internal/domain"
)

func TestNewMoney(t *testing.T) {
	tests := []struct {
		amount   string
		currency string
		units    int64
	}{
		{"0.00", "BRL", 0},
		{"0.01", "BRL", 1},
		{"0.10", "BRL", 10},
		{"1.00", "BRL", 100},
		{"25.00", "BRL", 2500},
		{"123456789.09", "BRL", 12345678909},
		{"25.00", "USD", 2500},
		{"25.00", "EUR", 2500},
		{"92233720368547758.07", "BRL", math.MaxInt64},
	}
	for _, tt := range tests {
		t.Run(tt.currency+"/"+tt.amount, func(t *testing.T) {
			money, err := domain.NewMoney(tt.amount, tt.currency)
			if err != nil {
				t.Fatal(err)
			}
			assertMoney(t, money, tt.units, tt.currency, tt.amount)
		})
	}
}

func TestNewMoneyRejectsInvalidAmounts(t *testing.T) {
	amounts := []string{
		"", "25", "25.", ".25", "25.0", "25.000", "25.001", "00.00", "025.00",
		"-1.00", "-0.00", "+1.00", " 1.00", "1.00 ", "1.00\n", "1 0.00",
		"1,00", "1_000.00", "1,000.00", "1.00.00", "1e2", "1E2", "1e2.00",
		"NaN", "Infinity", "Inf", "１.00", "1.０0", "1.0a",
	}
	for _, amount := range amounts {
		t.Run(amount, func(t *testing.T) {
			_, err := domain.NewMoney(amount, "BRL")
			if !errors.Is(err, domain.ErrInvalidAmount) {
				t.Fatalf("error = %v, want ErrInvalidAmount", err)
			}
		})
	}
}

func TestNewMoneyRejectsParsingOverflow(t *testing.T) {
	for _, amount := range []string{
		"92233720368547758.08",
		"92233720368547759.00",
		"99999999999999999999999999999999999999999999999999.99",
	} {
		t.Run(amount, func(t *testing.T) {
			_, err := domain.NewMoney(amount, "BRL")
			if !errors.Is(err, domain.ErrMoneyOverflow) {
				t.Fatalf("error = %v, want ErrMoneyOverflow", err)
			}
		})
	}
}

func TestConstructorsRejectInvalidCurrencies(t *testing.T) {
	for _, currency := range []string{"", "brl", "BrL", " BRL", "BRL ", "ZZZ", "BRA", "BR", "BRLL", "JPY"} {
		t.Run(currency, func(t *testing.T) {
			_, parseErr := domain.NewMoney("25.00", currency)
			_, unitsErr := domain.NewMoneyFromMinorUnits(2500, currency)
			_, zeroErr := domain.ZeroMoney(currency)
			for _, err := range []error{parseErr, unitsErr, zeroErr} {
				if !errors.Is(err, domain.ErrInvalidCurrency) {
					t.Errorf("error = %v, want ErrInvalidCurrency", err)
				}
			}
		})
	}
}

func TestZeroMoney(t *testing.T) {
	for _, currency := range []string{"BRL", "USD", "EUR"} {
		t.Run(currency, func(t *testing.T) {
			money, err := domain.ZeroMoney(currency)
			if err != nil {
				t.Fatal(err)
			}
			assertMoney(t, money, 0, currency, "0.00")
		})
	}
}

func TestInternalMoneyFormatting(t *testing.T) {
	tests := []struct {
		units  int64
		amount string
	}{
		{0, "0.00"},
		{1, "0.01"},
		{-1, "-0.01"},
		{-10, "-0.10"},
		{-100, "-1.00"},
		{-2500, "-25.00"},
		{math.MinInt64, "-92233720368547758.08"},
		{math.MaxInt64, "92233720368547758.07"},
	}
	for _, tt := range tests {
		t.Run(tt.amount, func(t *testing.T) {
			assertMoney(t, moneyFromUnits(t, tt.units), tt.units, "BRL", tt.amount)
		})
	}
}

func TestMoneyArithmeticIsExactAndImmutable(t *testing.T) {
	left, err := domain.NewMoney("0.10", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	right, err := domain.NewMoney("0.20", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	leftBefore, rightBefore := left, right

	sum, err := left.Add(right)
	if err != nil {
		t.Fatal(err)
	}
	assertMoney(t, sum, 30, "BRL", "0.30")

	difference, err := left.Subtract(right)
	if err != nil {
		t.Fatal(err)
	}
	assertMoney(t, difference, -10, "BRL", "-0.10")

	negative, err := right.Negate()
	if err != nil {
		t.Fatal(err)
	}
	assertMoney(t, negative, -20, "BRL", "-0.20")

	if left != leftBefore || right != rightBefore {
		t.Fatal("arithmetic modified its operands")
	}
}

func TestMoneyRejectsDifferentCurrencies(t *testing.T) {
	brl, err := domain.NewMoney("1.00", "BRL")
	if err != nil {
		t.Fatal(err)
	}
	usd, err := domain.NewMoney("1.00", "USD")
	if err != nil {
		t.Fatal(err)
	}
	_, addErr := brl.Add(usd)
	_, subtractErr := brl.Subtract(usd)
	_, compareErr := brl.Compare(usd)
	for _, err := range []error{addErr, subtractErr, compareErr} {
		if !errors.Is(err, domain.ErrCurrencyMismatch) {
			t.Errorf("error = %v, want ErrCurrencyMismatch", err)
		}
	}
}

func TestMoneyRejectsUninitializedValues(t *testing.T) {
	var invalid domain.Money
	valid := moneyFromUnits(t, 100)
	_, unitsErr := invalid.MinorUnits()
	_, currencyErr := invalid.Currency()
	_, amountErr := invalid.Amount()
	_, addErr := invalid.Add(valid)
	_, addOtherErr := valid.Add(invalid)
	_, subtractErr := invalid.Subtract(valid)
	_, subtractOtherErr := valid.Subtract(invalid)
	_, negateErr := invalid.Negate()
	_, compareErr := invalid.Compare(valid)
	_, compareOtherErr := valid.Compare(invalid)
	_, jsonErr := json.Marshal(invalid)

	for name, err := range map[string]error{
		"validate": invalid.Validate(), "units": unitsErr, "currency": currencyErr,
		"amount": amountErr, "add": addErr, "add other": addOtherErr,
		"subtract": subtractErr, "subtract other": subtractOtherErr, "negate": negateErr,
		"compare": compareErr, "compare other": compareOtherErr, "json": jsonErr,
	} {
		t.Run(name, func(t *testing.T) {
			if !errors.Is(err, domain.ErrInvalidMoney) {
				t.Fatalf("error = %v, want ErrInvalidMoney", err)
			}
		})
	}
}

func TestMoneyCompare(t *testing.T) {
	tests := []struct {
		left, right int64
		want        int
	}{
		{10, 20, -1}, {20, 10, 1}, {10, 10, 0}, {0, 0, 0},
		{-10, 0, -1}, {0, -10, 1}, {-20, -10, -1},
		{math.MinInt64, math.MaxInt64, -1},
		{math.MaxInt64, math.MinInt64, 1},
	}
	for _, tt := range tests {
		left, right := moneyFromUnits(t, tt.left), moneyFromUnits(t, tt.right)
		got, err := left.Compare(right)
		if err != nil || got != tt.want {
			t.Errorf("Compare(%d, %d) = %d, %v; want %d", tt.left, tt.right, got, err, tt.want)
		}
	}
}

func TestMoneyNegateBoundaries(t *testing.T) {
	for _, units := range []int64{0, 1, -1, math.MaxInt64, math.MinInt64 + 1} {
		got, err := moneyFromUnits(t, units).Negate()
		if err != nil {
			t.Fatal(err)
		}
		actual, err := got.MinorUnits()
		if err != nil || actual != -units {
			t.Fatalf("Negate(%d) = %d, %v", units, actual, err)
		}
	}
	_, err := moneyFromUnits(t, math.MinInt64).Negate()
	if !errors.Is(err, domain.ErrMoneyOverflow) {
		t.Fatalf("negating minimum int64: error = %v, want ErrMoneyOverflow", err)
	}
}

func TestMoneyMarshalJSON(t *testing.T) {
	tests := []struct {
		units int64
		want  string
	}{
		{2500, `{"amount":"25.00","currency":"BRL"}`},
		{0, `{"amount":"0.00","currency":"BRL"}`},
		{-1, `{"amount":"-0.01","currency":"BRL"}`},
		{math.MinInt64, `{"amount":"-92233720368547758.08","currency":"BRL"}`},
		{math.MaxInt64, `{"amount":"92233720368547758.07","currency":"BRL"}`},
	}
	for _, tt := range tests {
		encoded, err := json.Marshal(moneyFromUnits(t, tt.units))
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != tt.want {
			t.Errorf("JSON = %s, want %s", encoded, tt.want)
		}
	}
}

// big.Int fornece um resultado de referência sem os limites de int64.
// Os seeds exercitam limites; o fuzzing também pode explorar outros pares.
func FuzzMoneyArithmetic(f *testing.F) {
	for _, pair := range [][2]int64{
		{0, 0}, {10, 20}, {100, -200}, {-100, -200},
		{math.MaxInt64, 1}, {math.MinInt64, -1},
		{math.MinInt64, 1}, {math.MaxInt64, -1},
		{math.MaxInt64, math.MinInt64}, {math.MinInt64, math.MaxInt64},
		{0, math.MinInt64}, {math.MinInt64, math.MinInt64},
		{math.MaxInt64, math.MaxInt64},
	} {
		f.Add(pair[0], pair[1])
	}
	f.Fuzz(func(t *testing.T, leftUnits, rightUnits int64) {
		left, right := moneyFromUnits(t, leftUnits), moneyFromUnits(t, rightUnits)
		leftBefore, rightBefore := left, right
		bigLeft, bigRight := big.NewInt(leftUnits), big.NewInt(rightUnits)

		sum, sumErr := left.Add(right)
		assertArithmetic(t, sum, sumErr, new(big.Int).Add(bigLeft, bigRight))
		difference, differenceErr := left.Subtract(right)
		assertArithmetic(t, difference, differenceErr, new(big.Int).Sub(bigLeft, bigRight))

		if left != leftBefore || right != rightBefore {
			t.Fatal("arithmetic modified its operands")
		}
	})
}

func assertArithmetic(t *testing.T, got domain.Money, err error, want *big.Int) {
	t.Helper()
	if !want.IsInt64() {
		if !errors.Is(err, domain.ErrMoneyOverflow) {
			t.Fatalf("result outside int64: error = %v, want ErrMoneyOverflow", err)
		}
		if !errors.Is(got.Validate(), domain.ErrInvalidMoney) {
			t.Fatal("overflow returned usable Money")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	actual, unitsErr := got.MinorUnits()
	if unitsErr != nil || actual != want.Int64() {
		t.Fatalf("units = %d, %v; want %s", actual, unitsErr, want)
	}
	currency, currencyErr := got.Currency()
	if currencyErr != nil || currency != "BRL" {
		t.Fatalf("currency = %q, %v; want BRL", currency, currencyErr)
	}
}

func moneyFromUnits(t *testing.T, units int64) domain.Money {
	t.Helper()
	money, err := domain.NewMoneyFromMinorUnits(units, "BRL")
	if err != nil {
		t.Fatal(err)
	}
	return money
}

func assertMoney(t *testing.T, money domain.Money, units int64, currency, amount string) {
	t.Helper()
	if err := money.Validate(); err != nil {
		t.Fatal(err)
	}
	actualUnits, err := money.MinorUnits()
	if err != nil || actualUnits != units {
		t.Errorf("units = %d, %v; want %d", actualUnits, err, units)
	}
	actualCurrency, err := money.Currency()
	if err != nil || actualCurrency != currency {
		t.Errorf("currency = %q, %v; want %q", actualCurrency, err, currency)
	}
	actualAmount, err := money.Amount()
	if err != nil || actualAmount != amount {
		t.Errorf("amount = %q, %v; want %q", actualAmount, err, amount)
	}
}
