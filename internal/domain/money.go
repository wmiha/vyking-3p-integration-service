package domain

import (
	"strconv"
	"strings"
)

// iso4217MinorUnits is the fallback when the currencies table has no minor
// units (USD is NULL in the reference data). Guessing would risk reporting
// amounts off by a factor of 100, so unknown currencies are not guessed.
var iso4217MinorUnits = map[string]int{
	"AUD": 2, "BHD": 3, "BRL": 2, "CAD": 2, "CHF": 2, "CLP": 0, "CZK": 2, "DKK": 2,
	"EUR": 2, "GBP": 2, "HUF": 2, "IQD": 3, "ISK": 0, "JOD": 3, "JPY": 0, "KRW": 0,
	"KWD": 3, "LYD": 3, "MXN": 2, "NOK": 2, "NZD": 2, "OMR": 3, "PLN": 2, "RSD": 2,
	"SEK": 2, "TND": 3, "TRY": 2, "USD": 2, "VND": 0, "ZAR": 2,
}

// MinorUnits returns the number of decimal places of a currency: the
// database value when present, otherwise the ISO 4217 value. ok is false when
// neither is known.
func MinorUnits(alpha3 string, fromDB *uint8) (int, bool) {
	if fromDB != nil {
		return int(*fromDB), true
	}
	units, ok := iso4217MinorUnits[strings.ToUpper(alpha3)]
	return units, ok
}

// FormatMinor renders an amount in minor units as an exact decimal string in
// the major unit, without floating point: (12345, 2) -> "123.45",
// (-5, 2) -> "-0.05", (1000, 0) -> "1000", (1234, 3) -> "1.234".
func FormatMinor(minor int64, units int) string {
	neg := minor < 0
	// uint64 conversion handles math.MinInt64 without overflow.
	abs := uint64(minor)
	if neg {
		abs = -abs
	}

	digits := strconv.FormatUint(abs, 10)
	if units > 0 {
		if len(digits) <= units {
			digits = strings.Repeat("0", units-len(digits)+1) + digits
		}
		digits = digits[:len(digits)-units] + "." + digits[len(digits)-units:]
	}
	if neg {
		return "-" + digits
	}
	return digits
}
