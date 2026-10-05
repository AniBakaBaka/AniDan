// SPDX-License-Identifier: AGPL-3.0-only
package recognition

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// Offset safely evaluates a bounded arithmetic grammar. No Go/Python evaluation,
// names other than EP, function calls, exponentiation, or arbitrary code are allowed.
func Offset(ep int, expr string) (int, error) {
	expr = strings.ToUpper(strings.TrimSpace(expr))
	if len(expr) > 256 || expr == "" {
		return ep, fmt.Errorf("empty or excessive offset expression")
	}
	if !strings.Contains(expr, "EP") {
		n, e := strconv.Atoi(expr)
		if e != nil || n > 1000000 || n < -1000000 {
			return ep, fmt.Errorf("offset must be an integer delta or arithmetic using EP")
		}
		v := ep + n
		if v < 1 {
			v = 1
		}
		return v, nil
	}
	p := arithmetic{s: expr, ep: float64(ep)}
	v, e := p.sum()
	p.space()
	if e != nil {
		return ep, e
	}
	if p.i != len(p.s) {
		return ep, fmt.Errorf("unsupported arithmetic at position %d", p.i+1)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 1e9 {
		return ep, fmt.Errorf("offset result out of range")
	}
	if v < 1 {
		return 1, nil
	}
	return int(v), nil
}

type arithmetic struct {
	s        string
	i, depth int
	ep       float64
}

func (p *arithmetic) space() {
	for p.i < len(p.s) && unicode.IsSpace(rune(p.s[p.i])) {
		p.i++
	}
}
func (p *arithmetic) sum() (float64, error) {
	v, e := p.product()
	if e != nil {
		return v, e
	}
	for {
		p.space()
		if p.i == len(p.s) || p.s[p.i] != '+' && p.s[p.i] != '-' {
			return v, nil
		}
		op := p.s[p.i]
		p.i++
		x, e := p.product()
		if e != nil {
			return v, e
		}
		if op == '+' {
			v += x
		} else {
			v -= x
		}
	}
}
func (p *arithmetic) product() (float64, error) {
	v, e := p.atom()
	if e != nil {
		return v, e
	}
	for {
		p.space()
		if p.i == len(p.s) || !strings.ContainsRune("*/%", rune(p.s[p.i])) {
			return v, nil
		}
		op := p.s[p.i]
		p.i++
		floor := op == '/' && p.i < len(p.s) && p.s[p.i] == '/'
		if floor {
			p.i++
		}
		x, e := p.atom()
		if e != nil {
			return v, e
		}
		if x == 0 && (op == '/' || op == '%') {
			return v, fmt.Errorf("division by zero")
		}
		switch op {
		case '*':
			v *= x
		case '/':
			v /= x
			if floor {
				v = math.Floor(v)
			}
		case '%':
			v = v - math.Floor(v/x)*x
		}
		if math.Abs(v) > 1e12 {
			return v, fmt.Errorf("arithmetic overflow")
		}
	}
}
func (p *arithmetic) atom() (float64, error) {
	p.space()
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > 32 {
		return 0, fmt.Errorf("arithmetic nesting limit")
	}
	if p.i >= len(p.s) {
		return 0, fmt.Errorf("missing operand")
	}
	c := p.s[p.i]
	if c == '+' || c == '-' {
		p.i++
		v, e := p.atom()
		if c == '-' {
			v = -v
		}
		return v, e
	}
	if c == '(' {
		p.i++
		v, e := p.sum()
		p.space()
		if e != nil {
			return v, e
		}
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return 0, fmt.Errorf("missing closing parenthesis")
		}
		p.i++
		return v, nil
	}
	if strings.HasPrefix(p.s[p.i:], "EP") {
		p.i += 2
		return p.ep, nil
	}
	start := p.i
	for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		p.i++
	}
	if start == p.i {
		return 0, fmt.Errorf("unsupported arithmetic operand at position %d", p.i+1)
	}
	v, e := strconv.ParseFloat(p.s[start:p.i], 64)
	if e != nil || v > 1e9 {
		return 0, fmt.Errorf("integer operand out of range")
	}
	return v, nil
}
