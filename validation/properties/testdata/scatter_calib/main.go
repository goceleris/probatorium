// Command scatter_calib reproduces heapScatterCrit, the critical-value table of I-MEM-1's
// trough-scatter floor (validation/properties/i_mem_scatter.go).
//
// For n iid N(0,1) troughs at equal spacing it draws the slope statistic
//
//	T(n) = b / (sigmaD / sqrt(Sxx))
//
// where b is the least-squares slope, Sxx the sum of squared deviations of x, and sigmaD the trimmed
// scale of the median-centred successive differences (scatterTrim = 0.75, the same arithmetic as
// trimmedDiffScale in i_mem_scatter.go), and prints the 1 - alpha quantile (alpha = 0.001) of T(n)
// for n = 4..30, 4,000,000 draws per n, seed 1000+n. It then prints the any-look false-alarm rate of
// the table over nested windows of 4 to 15 troughs (0.75 % in the committed table).
//
// It lives under testdata so that `go build ./...` and the linters skip it. Run it with
//
//	go run ./validation/properties/testdata/scatter_calib
//
// (about a minute on 10 cores). The output of the committed run is the table in i_mem_scatter.go;
// the calibration test in i_mem_scatter_test.go spot-checks three entries by a smaller simulation.
package main

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"
)

const (
	alpha = 0.001
	trim  = 0.75
	nmax  = 30
	draws = 4_000_000
)

func median(v []float64) float64 {
	sort.Float64s(v)
	n := len(v)
	if n%2 == 1 {
		return v[n/2]
	}
	return (v[n/2-1] + v[n/2]) / 2
}

// trimmedScale mirrors trimmedDiffScale: centre the differences at their median, keep the smallest
// ceil(trim * len) squares, divide their mean by the Gaussian truncated-second-moment factor, take the
// square root and divide by sqrt(2).
func trimmedScale(d []float64) float64 {
	m := median(append([]float64(nil), d...))
	sq := make([]float64, len(d))
	for i, v := range d {
		sq[i] = (v - m) * (v - m)
	}
	sort.Float64s(sq)
	k := int(math.Ceil(trim * float64(len(sq))))
	var s float64
	for _, v := range sq[:k] {
		s += v
	}
	f := 1.0
	if k < len(sq) {
		p := float64(k) / float64(len(sq))
		z := math.Sqrt2 * math.Erfinv(p)
		f = 1 - 2*z*math.Exp(-z*z/2)/math.Sqrt(2*math.Pi)/p
	}
	return math.Sqrt(s/float64(k)/f) / math.Sqrt2
}

// stat is T for troughs y at x = 0..len(y)-1.
func stat(y []float64, d []float64) float64 {
	n := len(y)
	var my float64
	for _, v := range y {
		my += v
	}
	my /= float64(n)
	mx := float64(n-1) / 2
	var sxx, sxy float64
	for i, v := range y {
		dx := float64(i) - mx
		sxx += dx * dx
		sxy += dx * (v - my)
	}
	d = d[:0]
	for i := 1; i < n; i++ {
		d = append(d, y[i]-y[i-1])
	}
	s := trimmedScale(d)
	if s == 0 {
		return math.Inf(1)
	}
	return sxy / sxx / (s / math.Sqrt(sxx))
}

func main() {
	crit := make([]float64, nmax+1)
	var wg sync.WaitGroup
	for n := 4; n <= nmax; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(1000 + n)))
			ts := make([]float64, 0, draws)
			y := make([]float64, n)
			d := make([]float64, 0, n)
			for k := 0; k < draws; k++ {
				for i := range y {
					y[i] = rng.NormFloat64()
				}
				ts = append(ts, stat(y, d))
			}
			sort.Float64s(ts)
			crit[n] = ts[int(float64(draws)*(1-alpha))]
		}(n)
	}
	wg.Wait()
	fmt.Println("alpha", alpha, "draws", draws, "per n; seeds 1000+n")
	for n := 4; n <= nmax; n++ {
		fmt.Printf("%d %.3f\n", n, crit[n])
	}
	rng := rand.New(rand.NewSource(99))
	hit := 0
	const cells = 400000
	y := make([]float64, 15)
	d := make([]float64, 0, 15)
	for k := 0; k < cells; k++ {
		for i := range y {
			y[i] = rng.NormFloat64()
		}
		for n := 4; n <= 15; n++ {
			if stat(y[:n], d) > crit[n] {
				hit++
				break
			}
		}
	}
	fmt.Printf("any-look false alarm n=4..15: %.4f (%d cells)\n", float64(hit)/cells, cells)
}
