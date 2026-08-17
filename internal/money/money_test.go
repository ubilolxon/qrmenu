package money

import (
	"strconv"
	"testing"
)

// The zero-decimal treatment of UZS is a deliberate divergence from ISO 4217
// (which assigns it an exponent of 2). It is pinned here because it is the kind
// of decision someone will later "fix" back to the standard, and doing so would
// silently multiply every price on every menu by 100.
func TestUZSIsZeroDecimal(t *testing.T) {
	if got := Exponent("UZS"); got != 0 {
		t.Fatalf("Exponent(UZS) = %d, want 0 — see the comment on exponents", got)
	}
	if got := Format(45000, "UZS"); got != "45"+thousandsSep+"000" {
		t.Errorf("Format(45000, UZS) = %q, want a plain som amount", got)
	}
}

func TestExponent(t *testing.T) {
	tests := map[string]int{
		"UZS": 0,
		"JPY": 0,
		"KZT": 0,
		"USD": 2,
		"EUR": 2,
		"RUB": 2,
		"":    2, // unknown falls back rather than erroring
		"XYZ": 2,
		"uzs": 0, // lookup is case-insensitive
	}

	for currency, want := range tests {
		if got := Exponent(currency); got != want {
			t.Errorf("Exponent(%q) = %d, want %d", currency, got, want)
		}
	}
}

func TestFormat(t *testing.T) {
	const s = thousandsSep

	tests := []struct {
		name     string
		minor    int64
		currency string
		want     string
	}{
		{"zero, zero-decimal", 0, "UZS", "0"},
		{"zero, two-decimal", 0, "USD", "0.00"},
		{"under a thousand", 800, "UZS", "800"},
		{"exactly a thousand", 1000, "UZS", "1" + s + "000"},
		{"four digits", 8000, "UZS", "8" + s + "000"},
		{"five digits", 45000, "UZS", "45" + s + "000"},
		{"six digits", 250000, "UZS", "250" + s + "000"},
		{"seven digits", 2500000, "UZS", "2" + s + "500" + s + "000"},
		{"ten digits", 1234567890, "UZS", "1" + s + "234" + s + "567" + s + "890"},
		{"negative", -12000, "UZS", "-12" + s + "000"},

		{"cents", 1250, "USD", "12.50"},
		{"sub-unit is left-padded", 5, "USD", "0.05"},
		{"exactly one unit", 100, "USD", "1.00"},
		{"grouped with cents", 123456789, "USD", "1" + s + "234" + s + "567.89"},
		{"negative with cents", -50, "USD", "-0.50"},

		{"unknown currency uses two decimals", 1234, "XYZ", "12.34"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Format(tt.minor, tt.currency); got != tt.want {
				t.Errorf("Format(%d, %q) = %q, want %q", tt.minor, tt.currency, got, tt.want)
			}
		})
	}
}

// The separator must not be an ordinary space or a comma: a plain space gets
// broken across lines by the browser, and a comma reads as a decimal point in
// both Uzbek and Russian typography.
func TestThousandsSeparatorIsNarrowNoBreakSpace(t *testing.T) {
	if thousandsSep != " " {
		t.Errorf("thousandsSep = %q, want U+202F NARROW NO-BREAK SPACE", thousandsSep)
	}
}

func TestFormatWithCurrency(t *testing.T) {
	if got, want := FormatWithCurrency(45000, "uzs"), "45"+thousandsSep+"000 UZS"; got != want {
		t.Errorf("FormatWithCurrency() = %q, want %q — the code should be upper-cased", got, want)
	}
}

// Formatting must never lose a digit, whatever the magnitude.
func TestFormatRoundTripsDigits(t *testing.T) {
	amounts := []int64{
		1, 12, 123, 1234, 12345, 123456, 1234567,
		9223372036854775807, // int64 max, since price_minor is a bigint
	}

	for _, currency := range []string{"UZS", "USD"} {
		for _, amount := range amounts {
			got := Format(amount, currency)

			var digits []rune
			for _, r := range got {
				if r >= '0' && r <= '9' {
					digits = append(digits, r)
				}
			}

			// The digits that come out are the digits that went in, except that
			// a sub-unit amount is left-padded to fill the decimal places.
			want := len(strconv.FormatInt(amount, 10))
			if exp := Exponent(currency); exp > 0 && want < exp+1 {
				want = exp + 1
			}
			if len(digits) != want {
				t.Errorf("Format(%d, %s) = %q: %d digits, want %d",
					amount, currency, got, len(digits), want)
			}
			if string(digits) != strconv.FormatInt(amount, 10) && want == len(strconv.FormatInt(amount, 10)) {
				t.Errorf("Format(%d, %s) = %q: digits %q do not match the input",
					amount, currency, got, string(digits))
			}
		}
	}
}
