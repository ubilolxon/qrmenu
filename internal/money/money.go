// Package money formats the integer minor-unit amounts used throughout the
// schema.
package money

import (
	"strconv"
	"strings"
)

// exponents maps a currency to how many decimal places it is written with.
//
// ISO 4217 assigns UZS an exponent of 2 (100 tiyin to the som), but tiyin have
// been out of circulation for years and nothing in Uzbekistan is priced in
// them. Following the standard here would mean every price carrying two zeroes
// that no one types, reads, or charges. So UZS is treated as a zero-decimal
// currency: price_minor holds whole som.
//
// The consequence is worth stating plainly: price_minor is "the smallest unit
// this currency is actually used in", not always the ISO minor unit.
var exponents = map[string]int{
	"UZS": 0,
	"JPY": 0,
	"KRW": 0,
	"VND": 0,
	"CLP": 0,
	"ISK": 0,
	"KZT": 0, // tiyn are likewise not used in practice
}

// DefaultExponent covers USD, EUR, RUB and most others.
const DefaultExponent = 2

// Exponent reports the decimal places for a currency.
func Exponent(currency string) int {
	if e, ok := exponents[strings.ToUpper(currency)]; ok {
		return e
	}
	return DefaultExponent
}

// Format renders an amount for display: 2500000 UZS -> "2 500 000",
// 1250 USD -> "12.50".
//
// The thousands separator is a narrow no-break space, which is what both Uzbek
// and Russian typography use and which never gets confused with a decimal
// comma the way a period or comma separator can.
func Format(minor int64, currency string) string {
	exp := Exponent(currency)

	negative := minor < 0
	if negative {
		minor = -minor
	}

	digits := strconv.FormatInt(minor, 10)

	var whole, frac string
	if exp == 0 {
		whole = digits
	} else {
		// Left-pad so an amount smaller than one unit still renders as 0.05.
		for len(digits) <= exp {
			digits = "0" + digits
		}
		whole, frac = digits[:len(digits)-exp], digits[len(digits)-exp:]
	}

	var b strings.Builder
	if negative {
		b.WriteByte('-')
	}
	b.WriteString(group(whole))
	if frac != "" {
		b.WriteByte('.')
		b.WriteString(frac)
	}
	return b.String()
}

// FormatWithCurrency appends the currency code, which is what a menu shows when
// the restaurant has not configured a symbol.
func FormatWithCurrency(minor int64, currency string) string {
	return Format(minor, currency) + " " + strings.ToUpper(currency)
}

const thousandsSep = " " // narrow no-break space

func group(digits string) string {
	if len(digits) <= 3 {
		return digits
	}
	var b strings.Builder
	lead := len(digits) % 3
	if lead > 0 {
		b.WriteString(digits[:lead])
	}
	for i := lead; i < len(digits); i += 3 {
		if b.Len() > 0 {
			b.WriteString(thousandsSep)
		}
		b.WriteString(digits[i : i+3])
	}
	return b.String()
}
