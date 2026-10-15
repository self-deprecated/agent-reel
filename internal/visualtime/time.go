// Package visualtime defines the isolated visual-recording timing contract.
// It does not capture, render, persist, or certify physical-terminal fidelity.
package visualtime

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// Time is an exact, nonnegative segment-relative count of nanoseconds. No wall
// clock or floating-point media value can implicitly become a Time.
type Time int64

// ParseSeconds accepts bounded JSON decimal seconds exactly representable in
// nanoseconds. It never rounds a short-lived state out of existence. This is a
// new-format admission policy, not a change to archive.Validator.
func ParseSeconds(token string) (Time, error) {
	if len(token) == 0 || len(token) > 64 || strings.TrimSpace(token) != token {
		return 0, fmt.Errorf("source timestamp length/whitespace")
	}
	var number json.Number
	if json.Unmarshal([]byte(token), &number) != nil || number.String() != token {
		return 0, fmt.Errorf("source timestamp must be a JSON number")
	}
	if i := strings.IndexAny(token, "eE"); i >= 0 {
		e, err := strconv.Atoi(token[i+1:])
		if err != nil || e < -18 || e > 18 {
			return 0, fmt.Errorf("source timestamp exponent outside [-18,18]")
		}
	}
	r, ok := new(big.Rat).SetString(token)
	if !ok || r.Sign() < 0 {
		return 0, fmt.Errorf("negative or invalid source timestamp")
	}
	r.Mul(r, big.NewRat(1_000_000_000, 1))
	if !r.IsInt() || !r.Num().IsInt64() {
		return 0, fmt.Errorf("source timestamp exceeds exact nanosecond range/precision")
	}
	return Time(r.Num().Int64()), nil
}

func add(a, b Time) (Time, error) {
	if a < 0 || b < 0 || a > Time(math.MaxInt64)-b {
		return 0, fmt.Errorf("segment clock overflow")
	}
	return a + b, nil
}
