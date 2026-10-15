package qualify

import (
	"cmp"
	"fmt"
	"slices"
)

// NearestRank returns the nearest-rank percentile num/den of an ascending
// slice: the value at 1-based rank ceil(n·num/den), clamped to [1, n]. The
// rank is computed in integers, so p99.9 of 1,000 samples is exactly rank
// 999. It returns the zero value for an empty slice.
func NearestRank[T cmp.Ordered](sorted []T, num, den int64) T {
	var zero T
	n := int64(len(sorted))
	if n == 0 || den <= 0 || num < 0 {
		return zero
	}
	rank := (n*num + den - 1) / den
	rank = max(1, min(rank, n))
	return sorted[rank-1]
}

// Percentiles are the standard summaries of a latency sample.
type Percentiles[T cmp.Ordered] struct {
	N                   int
	P50, P99, P999, Max T
}

// Summarize sorts a copy of samples and returns p50/p99/p99.9/max,
// nearest-rank.
func Summarize[T cmp.Ordered](samples []T) Percentiles[T] {
	s := slices.Clone(samples)
	slices.Sort(s)
	p := Percentiles[T]{N: len(s)}
	if len(s) == 0 {
		return p
	}
	p.P50 = NearestRank(s, 50, 100)
	p.P99 = NearestRank(s, 99, 100)
	p.P999 = NearestRank(s, 999, 1000)
	p.Max = s[len(s)-1]
	return p
}

// Point is one (x, y) sample. X is exact integer logical nanoseconds.
type Point struct {
	X int64
	Y float64
}

// Slope is a least-squares fit over the samples in the last two-thirds of
// the sampled span.
type Slope struct {
	// PerHour is the fitted slope in y units per logical hour.
	PerHour float64
	// Intercept is y at X = 0 of the fit.
	Intercept float64
	// N is the number of samples in the window; From and To bound it.
	N        int
	From, To int64
}

// SlopeLastTwoThirds fits y against x over the samples with
// x >= first + ceil((last-first)/3), where first and last are the extreme x
// of all samples. At least two distinct x values must fall in the window.
func SlopeLastTwoThirds(points []Point) (Slope, error) {
	if len(points) < 2 {
		return Slope{}, fmt.Errorf("qualify: slope needs at least two samples, have %d", len(points))
	}
	first, last := points[0].X, points[0].X
	for _, p := range points {
		first, last = min(first, p.X), max(last, p.X)
	}
	cut := first + (last-first+2)/3
	var window []Point
	for _, p := range points {
		if p.X >= cut {
			window = append(window, p)
		}
	}
	s, err := LeastSquares(window)
	if err != nil {
		return Slope{}, err
	}
	s.From, s.To = cut, last
	return s, nil
}

// LeastSquares fits y = a + b·x over points and reports b per logical hour.
// x is shifted to the first point before squaring, to keep precision.
func LeastSquares(points []Point) (Slope, error) {
	if len(points) < 2 {
		return Slope{}, fmt.Errorf("qualify: least squares needs at least two samples, have %d", len(points))
	}
	origin := points[0].X
	var sx, sy, sxx, sxy float64
	n := float64(len(points))
	for _, p := range points {
		x := float64(p.X-origin) / float64(Hour)
		sx += x
		sy += p.Y
		sxx += x * x
		sxy += x * p.Y
	}
	d := n*sxx - sx*sx
	if d == 0 {
		return Slope{}, fmt.Errorf("qualify: least squares needs two distinct x values")
	}
	b := (n*sxy - sx*sy) / d
	a := (sy - b*sx) / n
	return Slope{PerHour: b, Intercept: a - b*float64(origin)/float64(Hour), N: len(points)}, nil
}
