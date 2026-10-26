package cli

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// sockEntry is one row of a kernel socket table, reduced to what port
// occupancy needs: the LOCAL address and port, and whether the row occupies
// the port for the given protocol. For TCP, Listening means state LISTEN. For
// UDP there is no listen state, so Listening means "bound": every UDP row
// (connected ones included) occupies its local port. Remote addresses and
// ports are never recorded, so a remote port can never be matched.
type sockEntry struct {
	Protocol  string // "tcp" or "udp"
	Addr      string
	Port      int
	Listening bool
}

// occupiedPorts returns the set of local ports of the given protocol that have
// a Listening (TCP) or bound (UDP) entry.
func occupiedPorts(entries []sockEntry, protocol string) map[int]bool {
	m := make(map[int]bool)
	for _, e := range entries {
		if e.Protocol == protocol && e.Listening {
			m[e.Port] = true
		}
	}
	return m
}

func validSockProtocol(p string) bool { return p == "tcp" || p == "udp" }

// parseProcNet parses /proc/net/tcp, tcp6, udp or udp6. v6 selects the 32-hex
// address form. The header line is skipped; blank lines, CRLF and trailing
// columns are tolerated; malformed rows are skipped. A table that has rows but
// no usable one, or that is entirely empty (a real table always has a header),
// is an error so an unreadable table is never mistaken for "no sockets".
//
// Addresses are decoded as the kernel prints them on a little-endian host:
// each 32-bit word is written as a native-endian hex number.
func parseProcNet(data []byte, protocol string, v6 bool) ([]sockEntry, error) {
	if !validSockProtocol(protocol) {
		return nil, fmt.Errorf("unsupported protocol %q", protocol)
	}
	var (
		res        []sockEntry
		nonBlank   int
		candidates int
	)
	addrLen := 8
	if v6 {
		addrLen = 32
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		nonBlank++
		if strings.Contains(line, "local_address") {
			continue // header
		}
		candidates++
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasSuffix(f[0], ":") {
			continue
		}
		addr, port, ok := decodeProcAddr(f[1], addrLen)
		if !ok || port < 1 {
			continue
		}
		listening := true
		if protocol == "tcp" {
			if len(f[3]) != 2 {
				continue
			}
			if _, err := hex.DecodeString(f[3]); err != nil {
				continue
			}
			listening = strings.EqualFold(f[3], "0A")
		}
		res = append(res, sockEntry{Protocol: protocol, Addr: addr, Port: port, Listening: listening})
	}
	if nonBlank == 0 {
		return nil, errors.New("empty socket table")
	}
	if candidates > 0 && len(res) == 0 {
		return nil, fmt.Errorf("socket table has %d row(s) but none could be parsed", candidates)
	}
	return res, nil
}

// decodeProcAddr decodes "ADDRHEX:PORTHEX" where ADDRHEX has addrLen hex
// digits (8 for IPv4, 32 for IPv6) and the port is exactly 4 hex digits.
func decodeProcAddr(s string, addrLen int) (addr string, port int, ok bool) {
	i := strings.IndexByte(s, ':')
	if i < 0 || i != addrLen || len(s)-i-1 != 4 {
		return "", 0, false
	}
	raw, err := hex.DecodeString(s[:i])
	if err != nil {
		return "", 0, false
	}
	p, err := strconv.ParseUint(s[i+1:], 16, 16)
	if err != nil {
		return "", 0, false
	}
	// Each 4-byte word is little-endian: reverse the bytes within every word.
	for w := 0; w+4 <= len(raw); w += 4 {
		raw[w], raw[w+1], raw[w+2], raw[w+3] = raw[w+3], raw[w+2], raw[w+1], raw[w]
	}
	if addrLen == 8 {
		return netip.AddrFrom4([4]byte(raw)).String(), int(p), true
	}
	return netip.AddrFrom16([16]byte(raw)).String(), int(p), true
}

// parseMacNetstat parses macOS `netstat -an -p tcp|udp`. Rows look like
// "tcp4 0 0 *.3000 *.* LISTEN" or "udp4 0 0 *.5353 *.*" (no state column).
// The port is the digits after the LAST '.' of the LOCAL address column;
// the foreign column is never inspected. Header lines and rows of other
// protocols are ignored; a "*.*" local address (unbound) yields no entry.
// Rows of the requested protocol that cannot be parsed are skipped, but if
// there are such rows and none parsed, an error is returned.
func parseMacNetstat(out []byte, protocol string) ([]sockEntry, error) {
	if !validSockProtocol(protocol) {
		return nil, fmt.Errorf("unsupported protocol %q", protocol)
	}
	var (
		res       []sockEntry
		rows, bad int
	)
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(strings.ToLower(f[0]), protocol) {
			continue
		}
		rows++
		minFields := 5
		if protocol == "tcp" {
			minFields = 6
		}
		if len(f) < minFields {
			bad++
			continue
		}
		local := f[3]
		dot := strings.LastIndexByte(local, '.')
		if dot < 0 || dot == len(local)-1 {
			bad++
			continue
		}
		portStr := local[dot+1:]
		if portStr == "*" {
			continue // unbound socket, e.g. "*.*"
		}
		port, perr := parseDecimalPort(portStr)
		if perr != nil {
			bad++
			continue
		}
		listening := true
		if protocol == "tcp" {
			listening = strings.EqualFold(f[5], "LISTEN")
		}
		res = append(res, sockEntry{Protocol: protocol, Addr: local[:dot], Port: port, Listening: listening})
	}
	if bad > 0 && len(res) == 0 && bad == rows {
		return nil, fmt.Errorf("netstat output has %d %s row(s) but none could be parsed", bad, protocol)
	}
	return res, nil
}

// parseLsofUDP parses `lsof -nP +c 0 -iUDP -FpcnT`. UDP has no TCP state, so
// every named socket counts; a connected socket ("a:5000->b:53") is attributed
// by its LOCAL port only. Results are de-duplicated by (port, pid) and sorted.
// Non-empty output that contains no process record is an error.
func parseLsofUDP(out []byte) ([]Listener, error) {
	var (
		res     []Listener
		pid     int
		haveP   bool
		command string
		sawP    bool
		nonBlk  bool
	)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		nonBlk = true
		val := line[1:]
		switch line[0] {
		case 'p':
			n, err := strconv.Atoi(strings.TrimSpace(val))
			haveP = err == nil && n > 0
			pid, command = n, ""
			if haveP {
				sawP = true
			}
		case 'c':
			command = strings.TrimSpace(val)
		case 'n':
			if !haveP {
				continue
			}
			name := strings.TrimSpace(val)
			if i := strings.Index(name, "->"); i >= 0 {
				name = name[:i]
			}
			if i := strings.IndexByte(name, ' '); i >= 0 {
				name = name[:i]
			}
			if _, port, ok := splitHostPort(name); ok {
				res = append(res, Listener{Port: port, PID: pid, Process: command, Addr: name})
			}
		}
	}
	if nonBlk && !sawP {
		return nil, errors.New("lsof output contains no process records")
	}
	return normalizeListeners(res), nil
}

// parseSSUDP parses `ss -lunp` (also tolerates a leading Netid column and
// connected "ESTAB" rows). Every socket row is a bound UDP socket. Rows without
// a users column are kept with PID 0 so the port still counts as occupied.
// Non-empty output with data rows but none parseable is an error.
func parseSSUDP(out []byte) ([]Listener, error) {
	var (
		res        []Listener
		candidates int
	)
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.EqualFold(f[0], "State") || strings.EqualFold(f[0], "Netid") {
			continue
		}
		candidates++
		base := 0
		if strings.EqualFold(f[0], "udp") {
			base = 1
		}
		if len(f) < base+5 {
			continue
		}
		local := f[base+3]
		_, port, ok := splitHostPort(local)
		if !ok {
			continue
		}
		matched := false
		if idx := strings.Index(line, "users:("); idx >= 0 {
			for _, m := range ssUserRe.FindAllStringSubmatch(line[idx:], -1) {
				pid, err := strconv.Atoi(m[2])
				if err != nil {
					continue
				}
				matched = true
				res = append(res, Listener{Port: port, PID: pid, Process: m[1], Addr: local})
			}
		}
		if !matched {
			res = append(res, Listener{Port: port, Addr: local})
		}
	}
	if candidates > 0 && len(res) == 0 {
		return nil, fmt.Errorf("ss output has %d row(s) but none could be parsed", candidates)
	}
	return normalizeListeners(res), nil
}
