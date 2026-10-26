package cli

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const procTCPHeader = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
const procTCP6Header = "  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

func procRow(local, remote, st string) string {
	return fmt.Sprintf("   0: %s %s %s 00000000:00000000 00:00000000 00000000     0        0 12345 1 0000000000000000 100 0 0 10 0\n", local, remote, st)
}

// skeys renders entries as "proto addr:port L|-" sorted.
func skeys(es []sockEntry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		l := "-"
		if e.Listening {
			l = "L"
		}
		out = append(out, fmt.Sprintf("%s %s:%d %s", e.Protocol, e.Addr, e.Port, l))
	}
	sort.Strings(out)
	return out
}

func sortedPorts(m map[int]bool) []int {
	out := []int{}
	for p := range m {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

func TestParseProcNetDecoding(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		v6       bool
		data     string
		want     []string
	}{
		{"ipv4 loopback 3000", "tcp", false, procTCPHeader + procRow("0100007F:0BB8", "00000000:0000", "0A"), []string{"tcp 127.0.0.1:3000 L"}},
		{"ipv4 wildcard 3000", "tcp", false, procTCPHeader + procRow("00000000:0BB8", "00000000:0000", "0A"), []string{"tcp 0.0.0.0:3000 L"}},
		{"ipv4 lan address", "tcp", false, procTCPHeader + procRow("030011AC:1F90", "00000000:0000", "0A"), []string{"tcp 172.17.0.3:8080 L"}},
		{"ipv6 any", "tcp", true, procTCP6Header + procRow("00000000000000000000000000000000:0BB8", "00000000000000000000000000000000:0000", "0A"), []string{"tcp :::3000 L"}},
		{"ipv6 loopback", "tcp", true, procTCP6Header + procRow("00000000000000000000000001000000:1F90", "00000000000000000000000000000000:0000", "0A"), []string{"tcp ::1:8080 L"}},
		{"ipv6 v4-mapped", "tcp", true, procTCP6Header + procRow("0000000000000000FFFF00000100007F:0BB8", "00000000000000000000000000000000:0000", "0A"), []string{"tcp ::ffff:127.0.0.1:3000 L"}},
		{"lowercase hex", "tcp", false, procTCPHeader + procRow("0100007f:0bb8", "00000000:0000", "0a"), []string{"tcp 127.0.0.1:3000 L"}},
		{"port is big-endian hex", "tcp", false, procTCPHeader + procRow("00000000:B80B", "00000000:0000", "0A"), []string{"tcp 0.0.0.0:47115 L"}},
		{"established not listening", "tcp", false, procTCPHeader + procRow("0100007F:0BB8", "0100007F:E83A", "01"), []string{"tcp 127.0.0.1:3000 -"}},
		{"time_wait not listening", "tcp", false, procTCPHeader + procRow("030011AC:8A2C", "843A52A7:0050", "06"), []string{"tcp 172.17.0.3:35372 -"}},
		{"syn_recv not listening", "tcp", false, procTCPHeader + procRow("0100007F:0BB8", "0100007F:E83A", "03"), []string{"tcp 127.0.0.1:3000 -"}},
		{"udp unconnected bound", "udp", false, procTCPHeader + procRow("00000000:270F", "00000000:0000", "07"), []string{"udp 0.0.0.0:9999 L"}},
		{"udp connected still bound", "udp", false, procTCPHeader + procRow("0100007F:14E9", "0100007F:DC50", "01"), []string{"udp 127.0.0.1:5353 L"}},
		{"udp6 bound", "udp", true, procTCP6Header + procRow("00000000000000000000000000000000:14EC", "00000000000000000000000000000000:0000", "07"), []string{"udp :::5356 L"}},
		{"header only", "tcp", false, procTCPHeader, nil},
		{"header only no trailing newline", "tcp", false, strings.TrimRight(procTCPHeader, "\n"), nil},
		{"blank lines tolerated", "tcp", false, "\n" + procTCPHeader + "\n\n" + procRow("0100007F:0BB8", "00000000:0000", "0A") + "\n", []string{"tcp 127.0.0.1:3000 L"}},
		{"crlf", "tcp", false, strings.ReplaceAll(procTCPHeader+procRow("0100007F:0BB8", "00000000:0000", "0A"), "\n", "\r\n"), []string{"tcp 127.0.0.1:3000 L"}},
		{"trailing fields ignored", "tcp", false, procTCPHeader + "   0: 0100007F:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 1 1 0000 100 0 0 10 0 extra fields here\n", []string{"tcp 127.0.0.1:3000 L"}},
		{"near-miss ports keep their own value", "tcp", false,
			procTCPHeader + procRow("00000000:32C8", "00000000:0000", "0A") + procRow("00000000:7531", "00000000:0000", "0A") + procRow("00000000:0BB8", "00000000:0000", "0A"),
			[]string{"tcp 0.0.0.0:13000 L", "tcp 0.0.0.0:30001 L", "tcp 0.0.0.0:3000 L"}},
		{"malformed rows skipped between good rows", "tcp", false,
			procTCPHeader +
				"garbage line\n" +
				procRow("0100007F:0BB8", "00000000:0000", "0A") +
				procRow("ZZZZZZZZ:0BB8", "00000000:0000", "0A") + // bad hex
				procRow("0100007F:0BB", "00000000:0000", "0A") + // short port
				procRow("0100007F:0000", "00000000:0000", "0A") + // port 0
				procRow("0100007:0BB8", "00000000:0000", "0A") + // short addr
				procRow("0100007F:1538", "00000000:0000", "XX") + // bad state
				"   9: 0100007F:1538\n" + // truncated
				"0100007F:1538 00000000:0000 0A\n" + // no index column
				procRow("0100007F:1538", "00000000:0000", "0A"),
			[]string{"tcp 127.0.0.1:3000 L", "tcp 127.0.0.1:5432 L"}},
		{"ipv6 row in v4 table is malformed", "tcp", false,
			procTCPHeader + procRow("00000000000000000000000001000000:1F90", "00000000:0000", "0A") + procRow("0100007F:0BB8", "00000000:0000", "0A"),
			[]string{"tcp 127.0.0.1:3000 L"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProcNet([]byte(tt.data), tt.protocol, tt.v6)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			g := skeys(got)
			w := append([]string{}, tt.want...)
			sort.Strings(w)
			if len(g) == 0 && len(w) == 0 {
				return
			}
			if !reflect.DeepEqual(g, w) {
				t.Errorf("got %v\nwant %v", g, w)
			}
		})
	}
}

func TestParseProcNetErrors(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		v6       bool
		data     string
	}{
		{"header and only malformed rows", "tcp", false, procTCPHeader + "junk\nmore junk here\n"},
		{"only wrong-width rows", "tcp", false, procTCPHeader + procRow("00000000000000000000000001000000:1F90", "00000000:0000", "0A")},
		{"only non-hex rows", "udp", false, procTCPHeader + procRow("QQQQQQQQ:0BB8", "00000000:0000", "07")},
		{"rows without any header, all malformed", "tcp", false, "not a table\n"},
		{"empty input", "tcp", false, ""},
		{"whitespace only", "tcp", false, "  \n\r\n"},
		{"nil-like", "udp", true, "\n"},
		{"unknown protocol", "sctp", false, procTCPHeader},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProcNet([]byte(tt.data), tt.protocol, tt.v6)
			if err == nil {
				t.Fatalf("expected error, got entries %v", skeys(got))
			}
		})
	}
}

func TestOccupiedPorts(t *testing.T) {
	es := []sockEntry{
		{Protocol: "tcp", Addr: "0.0.0.0", Port: 3000, Listening: true},
		{Protocol: "tcp", Addr: "127.0.0.1", Port: 52000, Listening: false},
		{Protocol: "tcp", Addr: "::1", Port: 8080, Listening: true},
		{Protocol: "udp", Addr: "0.0.0.0", Port: 5353, Listening: true},
		{Protocol: "tcp", Addr: "0.0.0.0", Port: 3000, Listening: true}, // duplicate
	}
	if got := sortedPorts(occupiedPorts(es, "tcp")); !reflect.DeepEqual(got, []int{3000, 8080}) {
		t.Errorf("tcp occupied = %v", got)
	}
	if got := sortedPorts(occupiedPorts(es, "udp")); !reflect.DeepEqual(got, []int{5353}) {
		t.Errorf("udp occupied = %v", got)
	}
	if got := occupiedPorts(nil, "tcp"); got == nil || len(got) != 0 {
		t.Errorf("nil entries must give an empty non-nil map, got %v", got)
	}
}

func TestParseMacNetstat(t *testing.T) {
	hdr := "Active Internet connections (including servers)\nProto Recv-Q Send-Q  Local Address          Foreign Address        (state)\n"
	tests := []struct {
		name     string
		protocol string
		data     string
		want     []string
	}{
		{"wildcard listen", "tcp", hdr + "tcp4       0      0  *.3000                 *.*                    LISTEN\n", []string{"tcp *:3000 L"}},
		{"ipv6 loopback", "tcp", hdr + "tcp6       0      0  ::1.8080               *.*                    LISTEN\n", []string{"tcp ::1:8080 L"}},
		{"ipv4 loopback", "tcp", hdr + "tcp4       0      0  127.0.0.1.5432         *.*                    LISTEN\n", []string{"tcp 127.0.0.1:5432 L"}},
		{"scoped link-local", "tcp", hdr + "tcp6       0      0  fe80::1%lo0.5000       *.*                    LISTEN\n", []string{"tcp fe80::1%lo0:5000 L"}},
		{"dual stack tcp46", "tcp", hdr + "tcp46      0      0  *.13000                *.*                    LISTEN\n", []string{"tcp *:13000 L"}},
		{"established not listening", "tcp", hdr + "tcp4       0      0  127.0.0.1.3000         127.0.0.1.52000        ESTABLISHED\n", []string{"tcp 127.0.0.1:3000 -"}},
		{"remote port 3000 is not local", "tcp", hdr + "tcp4       0      0  127.0.0.1.52000        127.0.0.1.3000         ESTABLISHED\n", []string{"tcp 127.0.0.1:52000 -"}},
		{"time_wait and close_wait", "tcp", hdr + "tcp4 0 0 192.0.2.10.52230 198.51.100.7.443 TIME_WAIT\ntcp4 0 0 127.0.0.1.9000 127.0.0.1.3000 CLOSE_WAIT\n", []string{"tcp 192.0.2.10:52230 -", "tcp 127.0.0.1:9000 -"}},
		{"lowercase state", "tcp", hdr + "tcp4 0 0 *.3000 *.* listen\n", []string{"tcp *:3000 L"}},
		{"crlf", "tcp", strings.ReplaceAll(hdr+"tcp4 0 0 *.3000 *.* LISTEN\n", "\n", "\r\n"), []string{"tcp *:3000 L"}},
		{"udp no state column", "udp", hdr + "udp4       0      0  *.5353                 *.*\n", []string{"udp *:5353 L"}},
		{"udp6", "udp", hdr + "udp6       0      0  *.5353                 *.*\nudp46 0 0 *.9999 *.*\n", []string{"udp *:5353 L", "udp *:9999 L"}},
		{"udp connected counts by local port only", "udp", hdr + "udp4       0      0  127.0.0.1.5000         192.0.2.53.3000\n", []string{"udp 127.0.0.1:5000 L"}},
		{"udp scoped", "udp", hdr + "udp6 0 0 fe80::1%lo0.123 *.*\n", []string{"udp fe80::1%lo0:123 L"}},
		{"unbound *.* yields nothing", "udp", hdr + "udp4 0 0 *.* *.*\nudp4 0 0 *.5353 *.*\n", []string{"udp *:5353 L"}},
		{"other protocol rows ignored", "tcp", hdr + "udp4 0 0 *.5353 *.*\ntcp4 0 0 *.3000 *.* LISTEN\n", []string{"tcp *:3000 L"}},
		{"header only", "tcp", hdr, nil},
		{"empty", "udp", "", nil},
		{"malformed skipped among good", "tcp", hdr + "tcp4 0 0 *.abc *.* LISTEN\ntcp4 0 0 noport *.* LISTEN\ntcp4 0 0 *.3000 *.* LISTEN\ntcp4 0 0 *.0 *.* LISTEN\ntcp4 0 0 *.70000 *.* LISTEN\ntcp4 0 0 *.+80 *.* LISTEN\n", []string{"tcp *:3000 L"}},
		{"near-miss ports", "tcp", hdr + "tcp4 0 0 *.13000 *.* LISTEN\ntcp4 0 0 *.30001 *.* LISTEN\n", []string{"tcp *:13000 L", "tcp *:30001 L"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMacNetstat([]byte(tt.data), tt.protocol)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			g := skeys(got)
			w := append([]string{}, tt.want...)
			sort.Strings(w)
			if len(g) == 0 && len(w) == 0 {
				return
			}
			if !reflect.DeepEqual(g, w) {
				t.Errorf("got %v\nwant %v", g, w)
			}
		})
	}
}

func TestParseMacNetstatErrors(t *testing.T) {
	hdr := "Active Internet connections (including servers)\nProto Recv-Q Send-Q  Local Address          Foreign Address        (state)\n"
	for name, tt := range map[string]struct{ proto, data string }{
		"only malformed tcp rows": {"tcp", hdr + "tcp4 0 0 garbage\ntcp4 0 0 *.xyz *.* LISTEN\n"},
		"tcp row missing state":   {"tcp", hdr + "tcp4 0 0 *.3000 *.*\n"},
		"only malformed udp rows": {"udp", hdr + "udp4 0 0 nodot *.*\n"},
		"unknown protocol":        {"sctp", hdr},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := parseMacNetstat([]byte(tt.data), tt.proto); err == nil {
				t.Fatalf("expected error, got %v", skeys(got))
			}
		})
	}
}

func TestParseLsofUDPInline(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"wildcard", "p100\ncmdns\nn*:5353\n", []string{"5353:100:mdns"}},
		{"ipv6 bracketed", "p100\ncdns\nn[::]:5353\nn[::1]:5354\n", []string{"5353:100:dns", "5354:100:dns"}},
		{"connected counts by local port", "p200\ncclient\nn127.0.0.1:5000->10.0.0.1:53\n", []string{"5000:200:client"}},
		{"remote port 3000 never selected", "p200\ncclient\nn127.0.0.1:52000->10.0.0.1:3000\n", []string{"52000:200:client"}},
		{"connected ipv6", "p200\ncc\nn[::1]:5000->[::1]:3000\n", []string{"5000:200:c"}},
		{"dedupe port and pid", "p100\ncx\nn*:5353\nn*:5353\nn[::]:5353\n", []string{"5353:100:x"}},
		{"same port two pids", "p100\ncx\nn*:5353\np101\ncy\nn*:5353\n", []string{"5353:100:x", "5353:101:y"}},
		{"names with spaces in command", "p7\ncMy App Helper\nn*:9999\n", []string{"9999:7:My App Helper"}},
		{"unbound and unparsable names skipped", "p7\ncx\nn*:*\nnnotaport\nn*:9999\n", []string{"9999:7:x"}},
		{"trailing space-annotation stripped", "p7\ncx\nn*:9999 (Idle)\n", []string{"9999:7:x"}},
		{"extra field lines ignored", "p7\ncx\nf9\nn*:9999\nTQR=0\nTQS=0\nTST=UNKNOWN\n", []string{"9999:7:x"}},
		{"invalid pid ignored", "pabc\ncx\nn*:9999\np8\ncy\nn*:9998\n", []string{"9998:8:y"}},
		{"crlf", "p7\r\ncx\r\nn*:9999\r\n", []string{"9999:7:x"}},
		{"empty output", "", nil},
		{"blank only", "\n\n", nil},
		{"process with no sockets", "p7\ncx\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLsofUDP([]byte(tt.in))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			fxAssertSet(t, got, tt.want...)
		})
	}
	if _, err := parseLsofUDP([]byte("lsof: WARNING: can't stat() fuse file system\n")); err == nil {
		t.Error("output without any process record must be an error")
	}
}

func TestParseSSUDPInline(t *testing.T) {
	hdr := "State  Recv-Q Send-Q Local Address:Port Peer Address:PortProcess\n"
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"unconnn with users", hdr + `UNCONN 0      0            0.0.0.0:9999      0.0.0.0:*    users:(("python3",pid=281,fd=3))` + "\n", []string{"9999:281:python3"}},
		{"ipv6 wildcard star", hdr + `UNCONN 0      0                  *:5356            *:*    users:(("python3",pid=281,fd=4))` + "\n", []string{"5356:281:python3"}},
		{"bracketed ipv6", hdr + `UNCONN 0 0 [::1]:5353 [::]:* users:(("avahi",pid=9,fd=1))` + "\n", []string{"5353:9:avahi"}},
		{"interface scoped", hdr + `UNCONN 0 0 127.0.0.53%lo:53 0.0.0.0:* users:(("resolved",pid=5,fd=13))` + "\n", []string{"53:5:resolved"}},
		{"no users column kept with pid 0", hdr + "UNCONN 0 0 0.0.0.0:5353 0.0.0.0:*\n", []string{"5353:0:"}},
		{"multiple users", hdr + `UNCONN 0 0 0.0.0.0:5353 0.0.0.0:* users:(("a",pid=1,fd=3),("b",pid=2,fd=4))` + "\n", []string{"5353:1:a", "5353:2:b"}},
		{"connected ESTAB counts by local port", hdr + `ESTAB 0 0 127.0.0.1:5000 10.0.0.1:3000 users:(("c",pid=3,fd=3))` + "\n", []string{"5000:3:c"}},
		{"peer port 3000 only never selects 3000", hdr + `UNCONN 0 0 0.0.0.0:52000 10.0.0.1:3000 users:(("c",pid=3,fd=3))` + "\n", []string{"52000:3:c"}},
		{"with netid column", "Netid State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n" + `udp UNCONN 0 0 0.0.0.0:9999 0.0.0.0:* users:(("p",pid=4,fd=3))` + "\n", []string{"9999:4:p"}},
		{"command with escaped quote", hdr + `UNCONN 0 0 0.0.0.0:9999 0.0.0.0:* users:(("we\"ird",pid=4,fd=3))` + "\n", []string{`9999:4:we\"ird`}},
		{"dedupe", hdr + `UNCONN 0 0 0.0.0.0:9999 0.0.0.0:* users:(("p",pid=4,fd=3))` + "\n" + `UNCONN 0 0 [::]:9999 [::]:* users:(("p",pid=4,fd=4))` + "\n", []string{"9999:4:p"}},
		{"crlf", strings.ReplaceAll(hdr+`UNCONN 0 0 0.0.0.0:9999 0.0.0.0:* users:(("p",pid=4,fd=3))`+"\n", "\n", "\r\n"), []string{"9999:4:p"}},
		{"header only", hdr, nil},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSSUDP([]byte(tt.in))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			fxAssertSet(t, got, tt.want...)
		})
	}
	if _, err := parseSSUDP([]byte(hdr + "this is not a socket row at all\n")); err == nil {
		t.Error("only unparseable rows must be an error")
	}
}

// ---- real and synthetic fixtures ----

func parseFixtureProc(t *testing.T, file, proto string, v6 bool) []sockEntry {
	t.Helper()
	es, err := parseProcNet([]byte(fxRead(t, "sockets/"+file)), proto, v6)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return es
}

func TestFixtureLinuxProcNetTCP(t *testing.T) {
	es := parseFixtureProc(t, "linux_proc_net_tcp.txt", "tcp", false)
	if len(es) != 6 {
		t.Fatalf("want 6 rows, got %d: %v", len(es), skeys(es))
	}
	occ := sortedPorts(occupiedPorts(es, "tcp"))
	if !reflect.DeepEqual(occ, []int{3000, 5432}) {
		t.Errorf("listening ports = %v, want [3000 5432]", occ)
	}
	for _, e := range es {
		if e.Port == 3000 && e.Listening && e.Addr != "0.0.0.0" {
			t.Errorf("3000 listener addr = %s, want 0.0.0.0", e.Addr)
		}
		if e.Port == 5432 && e.Addr != "127.0.0.1" {
			t.Errorf("5432 addr = %s, want 127.0.0.1", e.Addr)
		}
	}
	// The server side of the held connection has local port 3000 but is
	// ESTABLISHED; the client side has remote port 3000 and an ephemeral local.
	var est3000, tw int
	for _, e := range es {
		if e.Port == 3000 && !e.Listening {
			est3000++
		}
		if e.Addr == "172.17.0.3" && !e.Listening {
			tw++
		}
	}
	if est3000 != 1 || tw != 2 {
		t.Errorf("established-to-3000 rows = %d (want 1), time_wait rows = %d (want 2)", est3000, tw)
	}
	occ13000 := occupiedPorts(es, "tcp")
	for _, p := range []int{13000, 30001, 300, 55544} {
		if occ13000[p] {
			t.Errorf("port %d must not be occupied", p)
		}
	}
}

func TestFixtureLinuxProcNetTCP6(t *testing.T) {
	es := parseFixtureProc(t, "linux_proc_net_tcp6.txt", "tcp", true)
	if got := skeys(es); !reflect.DeepEqual(got, []string{"tcp ::1:8080 L"}) {
		t.Errorf("got %v", got)
	}
}

func TestFixtureLinuxProcNetUDP(t *testing.T) {
	es := parseFixtureProc(t, "linux_proc_net_udp.txt", "udp", false)
	if got := skeys(es); !reflect.DeepEqual(got, []string{"udp 0.0.0.0:9999 L", "udp 127.0.0.1:5353 L"}) {
		t.Errorf("udp: got %v", got)
	}
	es6 := parseFixtureProc(t, "linux_proc_net_udp6.txt", "udp", true)
	if got := skeys(es6); !reflect.DeepEqual(got, []string{"udp :::5356 L"}) {
		t.Errorf("udp6: got %v", got)
	}
	// The connected socket's remote port (0xDC50) must not occupy anything.
	if occupiedPorts(es, "udp")[0xDC50] {
		t.Error("remote port of a connected UDP socket was treated as occupied")
	}
}

func TestFixtureLinuxLsofUDP(t *testing.T) {
	got, err := parseLsofUDP([]byte(fxRead(t, "sockets/linux_lsof_udp.txt")))
	if err != nil {
		t.Fatal(err)
	}
	fxAssertSet(t, got, "5353:280:nc", "5356:281:python3", "9999:281:python3")
	for _, l := range got {
		if l.Port == 56400 {
			t.Errorf("remote port of connected UDP socket selected: %+v", l)
		}
	}
}

func TestFixtureLinuxSSUDP(t *testing.T) {
	got, err := parseSSUDP([]byte(fxRead(t, "sockets/linux_ss_lunp.txt")))
	if err != nil {
		t.Fatal(err)
	}
	// The connected nc (pid 280) is absent from `ss -lunp`: that is why the
	// socket table is a separate occupancy source from owner attribution.
	fxAssertSet(t, got, "5356:281:python3", "9999:281:python3")
}

func TestFixtureLinuxSSLtnpUnchangedByUDPParser(t *testing.T) {
	// parseSS (existing) on the new capture, for cross-checking the roles table.
	fxAssertSet(t, parseSS(fxRead(t, "sockets/linux_ss_ltnp.txt")),
		"3000:277:python3", "5432:279:nc", "8080:278:python3")
}

func TestFixtureSocketTableVsOwnersAgree(t *testing.T) {
	udp := parseFixtureProc(t, "linux_proc_net_udp.txt", "udp", false)
	udp6 := parseFixtureProc(t, "linux_proc_net_udp6.txt", "udp", true)
	occ := occupiedPorts(append(udp, udp6...), "udp")
	owners, err := parseLsofUDP([]byte(fxRead(t, "sockets/linux_lsof_udp.txt")))
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range owners {
		if !occ[l.Port] {
			t.Errorf("lsof owner of port %d missing from socket table", l.Port)
		}
	}
	if len(occ) != 3 {
		t.Errorf("occupied udp ports = %v", sortedPorts(occ))
	}
}

func TestFixtureDarwinNetstatTCP(t *testing.T) {
	es, err := parseMacNetstat([]byte(fxRead(t, "sockets/darwin_netstat_tcp.txt")), "tcp")
	if err != nil {
		t.Fatal(err)
	}
	occ := sortedPorts(occupiedPorts(es, "tcp"))
	want := []int{3000, 5000, 5432, 8080, 13000, 30001}
	if !reflect.DeepEqual(occ, want) {
		t.Errorf("listening = %v, want %v", occ, want)
	}
	// 52000/52114/52230/9000 appear only as established/closing rows.
	m := occupiedPorts(es, "tcp")
	for _, p := range []int{52000, 52114, 52230, 9000, 443} {
		if m[p] {
			t.Errorf("port %d must not be occupied", p)
		}
	}
	var sawScoped bool
	for _, e := range es {
		if e.Port == 5000 && e.Addr == "fe80::1%lo0" {
			sawScoped = true
		}
	}
	if !sawScoped {
		t.Error("scoped link-local listener not decoded")
	}
}

func TestFixtureDarwinNetstatUDP(t *testing.T) {
	es, err := parseMacNetstat([]byte(fxRead(t, "sockets/darwin_netstat_udp.txt")), "udp")
	if err != nil {
		t.Fatal(err)
	}
	occ := sortedPorts(occupiedPorts(es, "udp"))
	if want := []int{123, 5000, 5353, 9999}; !reflect.DeepEqual(occ, want) {
		t.Errorf("bound = %v, want %v", occ, want)
	}
	if occupiedPorts(es, "udp")[3000] {
		t.Error("remote port 3000 of a connected UDP socket was selected")
	}
}

// TestRemotePortNeverSelected states the contract across all table parsers:
// a remote (peer) port equal to 3000 never occupies local port 3000.
func TestRemotePortNeverSelected(t *testing.T) {
	proc, err := parseProcNet([]byte(procTCPHeader+
		procRow("0100007F:E83A", "0100007F:0BB8", "01")+ // local 59450 -> remote 3000
		procRow("030011AC:8A2C", "0100007F:0BB8", "06")), "tcp", false)
	if err != nil {
		t.Fatal(err)
	}
	procUDP, err := parseProcNet([]byte(procTCPHeader+procRow("0100007F:14E9", "0100007F:0BB8", "01")), "udp", false)
	if err != nil {
		t.Fatal(err)
	}
	mac, err := parseMacNetstat([]byte("tcp4 0 0 127.0.0.1.52000 127.0.0.1.3000 ESTABLISHED\ntcp4 0 0 127.0.0.1.52001 *.3000 LISTEN\n"), "tcp")
	if err != nil {
		t.Fatal(err)
	}
	macUDP, err := parseMacNetstat([]byte("udp4 0 0 127.0.0.1.5000 192.0.2.53.3000\n"), "udp")
	if err != nil {
		t.Fatal(err)
	}
	lsof, err := parseLsofUDP([]byte("p1\ncx\nn127.0.0.1:5000->10.0.0.1:3000\n"))
	if err != nil {
		t.Fatal(err)
	}
	ss, err := parseSSUDP([]byte("State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\nUNCONN 0 0 127.0.0.1:5000 10.0.0.1:3000 users:((\"x\",pid=1,fd=3))\n"))
	if err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]map[int]bool{
		"proc tcp": occupiedPorts(proc, "tcp"),
		"proc udp": occupiedPorts(procUDP, "udp"),
		"mac tcp":  occupiedPorts(mac, "tcp"),
		"mac udp":  occupiedPorts(macUDP, "udp"),
	} {
		if m[3000] {
			t.Errorf("%s: port 3000 occupied via a remote port", name)
		}
	}
	for name, ls := range map[string][]Listener{"lsof udp": lsof, "ss udp": ss} {
		for _, l := range ls {
			if l.Port == 3000 {
				t.Errorf("%s: selected remote port 3000: %+v", name, l)
			}
		}
	}
}
