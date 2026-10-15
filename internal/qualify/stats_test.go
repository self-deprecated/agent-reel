package qualify

import (
	"math"
	"testing"
)

func TestNearestRank(t *testing.T) {
	s := make([]int64, 1000)
	for i := range s {
		s[i] = int64(i + 1)
	}
	if NearestRank(s, 99, 100) != 990 || NearestRank(s, 999, 1000) != 999 || NearestRank(s, 50, 100) != 500 || NearestRank(s, 100, 100) != 1000 {
		t.Fatal("nearest rank on 1..1000")
	}
	if NearestRank([]int64{7}, 999, 1000) != 7 || NearestRank([]int64{}, 99, 100) != 0 || NearestRank(s, 0, 100) != 1 {
		t.Fatal("edge ranks")
	}
	p := Summarize([]int32{5, 1, 4, 2, 3})
	if p.N != 5 || p.P50 != 3 || p.P99 != 5 || p.P999 != 5 || p.Max != 5 {
		t.Fatalf("summary %+v", p)
	}
}

func TestSlopeLastTwoThirds(t *testing.T) {
	// y = 5 MiB/h after a warm-up spike in the first third, which the
	// window must exclude.
	var pts []Point
	for h := int64(0); h <= 72; h++ {
		y := float64(5 * h)
		if h < 24 {
			y += 1000
		}
		pts = append(pts, Point{X: h * Hour, Y: y})
	}
	s, err := SlopeLastTwoThirds(pts)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(s.PerHour-5) > 1e-9 || s.From != 24*Hour || s.To != 72*Hour || s.N != 49 {
		t.Fatalf("slope %+v", s)
	}
	if math.Abs(s.Intercept) > 1e-6 {
		t.Fatalf("intercept %v", s.Intercept)
	}
	if _, err := SlopeLastTwoThirds([]Point{{0, 1}}); err == nil {
		t.Fatal("one sample accepted")
	}
	if _, err := LeastSquares([]Point{{5, 1}, {5, 2}}); err == nil {
		t.Fatal("degenerate x accepted")
	}
}
