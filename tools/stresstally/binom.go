package main

import "math"

// clopperPearson returns the exact (Clopper-Pearson) two-sided 95% confidence
// interval for a failure rate of k failures in n runs. It is the interval
// built from the binomial distribution itself rather than a normal
// approximation, so it stays honest at the small counts and the rates near 0
// that a flake measurement produces: 0 failures in 100 runs is "at most
// 3.6%", not "exactly 0".
//
// n == 0 (a test that only ever skipped) has no interval; both bounds are NaN.
func clopperPearson(k, n int) (lo, hi float64) {
	if n <= 0 || k < 0 || k > n {
		return math.NaN(), math.NaN()
	}
	const alpha = 0.05
	lo, hi = 0, 1
	if k > 0 {
		lo = betaQuantile(alpha/2, float64(k), float64(n-k+1))
	}
	if k < n {
		hi = betaQuantile(1-alpha/2, float64(k+1), float64(n-k))
	}
	return lo, hi
}

// betaQuantile inverts the regularized incomplete beta function by
// bisection: the x in [0, 1] with I_x(a, b) = p. I_x is monotone in x, so
// bisection cannot miss, and 200 halvings are far past float64 resolution.
func betaQuantile(p, a, b float64) float64 {
	lo, hi := 0.0, 1.0
	for range 200 {
		mid := (lo + hi) / 2
		if mid == lo || mid == hi {
			break
		}
		if regIncBeta(mid, a, b) < p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// regIncBeta is the regularized incomplete beta function I_x(a, b), by the
// continued fraction (modified Lentz), using the symmetry relation where the
// fraction converges slowly.
func regIncBeta(x, a, b float64) float64 {
	switch {
	case x <= 0:
		return 0
	case x >= 1:
		return 1
	}
	la, _ := math.Lgamma(a)
	lb, _ := math.Lgamma(b)
	lab, _ := math.Lgamma(a + b)
	front := math.Exp(a*math.Log(x) + b*math.Log1p(-x) + lab - la - lb)
	if x < (a+1)/(a+b+2) {
		return front * betaCF(x, a, b) / a
	}
	return 1 - front*betaCF(1-x, b, a)/b
}

func betaCF(x, a, b float64) float64 {
	const (
		maxIter = 1000
		eps     = 1e-16
		tiny    = 1e-300
	)
	qab, qap, qam := a+b, a+1, a-1
	c, d := 1.0, 1-qab*x/qap
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= maxIter; m++ {
		fm := float64(m)
		m2 := 2 * fm
		aa := fm * (b - fm) * x / ((qam + m2) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c
		aa = -(a + fm) * (qab + fm) * x / ((a + m2) * (qap + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < eps {
			break
		}
	}
	return h
}
