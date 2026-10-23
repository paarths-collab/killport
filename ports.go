package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const maxPort = 65535

// parseDecimalPort parses a strictly decimal TCP port in 1..65535. Signs,
// whitespace, hex and anything else are rejected.
func parseDecimalPort(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty port")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%q is not a decimal number", s)
		}
	}
	if len(s) > 9 {
		return 0, fmt.Errorf("port %q is out of range (1-%d)", s, maxPort)
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > maxPort {
		return 0, fmt.Errorf("port %q is out of range (1-%d)", s, maxPort)
	}
	return n, nil
}

// parsePortSpecs turns CLI arguments ("3000", "3000-3005") into a sorted,
// de-duplicated list of ports. Every spec is validated before any range is
// expanded, so allocation is bounded by 65535 entries. Any invalid spec fails
// the whole invocation.
func parsePortSpecs(args []string) ([]int, error) {
	seen := make(map[int]bool)
	for _, arg := range args {
		switch strings.Count(arg, "-") {
		case 0:
			n, err := parseDecimalPort(arg)
			if err != nil {
				return nil, fmt.Errorf("invalid port %q: %v", arg, err)
			}
			seen[n] = true
		case 1:
			parts := strings.SplitN(arg, "-", 2)
			lo, err := parseDecimalPort(parts[0])
			if err != nil {
				return nil, fmt.Errorf("invalid port range %q: %v", arg, err)
			}
			hi, err := parseDecimalPort(parts[1])
			if err != nil {
				return nil, fmt.Errorf("invalid port range %q: %v", arg, err)
			}
			if lo > hi {
				return nil, fmt.Errorf("invalid port range %q: start is greater than end", arg)
			}
			for p := lo; p <= hi; p++ {
				seen[p] = true
			}
		default:
			return nil, fmt.Errorf("invalid port spec %q: expected N or START-END", arg)
		}
	}
	ports := make([]int, 0, len(seen))
	for p := range seen {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return ports, nil
}
