package main

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestParsePortSpecs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    []int
		wantErr bool
	}{
		{"single", []string{"3000"}, []int{3000}, false},
		{"min", []string{"1"}, []int{1}, false},
		{"max", []string{"65535"}, []int{65535}, false},
		{"range", []string{"3000-3002"}, []int{3000, 3001, 3002}, false},
		{"single-range", []string{"3000-3000"}, []int{3000}, false},
		{"dedupe+sort", []string{"3002", "3000-3001", "3001", "3000"}, []int{3000, 3001, 3002}, false},
		{"zero", []string{"0"}, nil, true},
		{"too big", []string{"65536"}, nil, true},
		{"reversed", []string{"5000-3000"}, nil, true},
		{"plus sign", []string{"+3000"}, nil, true},
		{"negative", []string{"-5"}, nil, true},
		{"open range", []string{"3000-"}, nil, true},
		{"open start", []string{"-3000"}, nil, true},
		{"huge range", []string{"1-99999999999"}, nil, true},
		{"huge single", []string{"99999999999999999999"}, nil, true},
		{"range past max", []string{"65530-65536"}, nil, true},
		{"range zero start", []string{"0-10"}, nil, true},
		{"garbage", []string{"abc"}, nil, true},
		{"comma", []string{"3000,8080"}, nil, true},
		{"three parts", []string{"1-2-3"}, nil, true},
		{"hex", []string{"0x50"}, nil, true},
		{"space", []string{" 80"}, nil, true},
		{"empty", []string{""}, nil, true},
		{"one bad poisons all", []string{"3000", "nope"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePortSpecs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestParsePortSpecsFullRangeBounded(t *testing.T) {
	got, err := parsePortSpecs([]string{"1-65535"})
	if err != nil || len(got) != 65535 {
		t.Fatalf("len=%d err=%v", len(got), err)
	}
}

func TestParseLsof(t *testing.T) {
	out := "" +
		"p100\ncnode\nf20\nn*:3000\nTST=LISTEN\nf21\nn[::1]:3000\nTST=LISTEN\n" + // dup pid on v4+v6
		"p200\ncpostgres\nn127.0.0.1:5432\nTST=LISTEN\nTQR=0\nTQS=0\n" + // no f lines, TST after n
		"p300\ncGoogle Chrome Helper\nf30\nn127.0.0.1:52000->127.0.0.1:3000\nTST=ESTABLISHED\n" + // connection
		"p400\ncnginx\nn*:13000\nTST=LISTEN\nn[::]:30001\nTST=LISTEN\n" +
		"p500\ncweird\nf8\nn*:4000\nTST=CLOSE_WAIT\n" + // non-LISTEN state
		"p600\ncmixed\nn*:6000\nTST=LISTEN\nn127.0.0.1:6000->127.0.0.1:41000\nTST=ESTABLISHED\n" +
		"p700\ncnostate\nn*:7000\n" // no TST at all, still a listener (-sTCP:LISTEN filtered)
	got := parseLsof(out)
	want := []Listener{
		{3000, 100, "node", "*:3000"},
		{5432, 200, "postgres", "127.0.0.1:5432"},
		{13000, 400, "nginx", "*:13000"},
		{30001, 400, "nginx", "[::]:30001"},
		{6000, 600, "mixed", "*:6000"},
		{7000, 700, "nostate", "*:7000"},
	}
	// 3000 appears once for pid 100 (v4 and v6 deduped).
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	for _, l := range got {
		if l.Port == 52000 || (l.Port == 3000 && l.PID == 300) || l.Port == 4000 || l.Port == 41000 {
			t.Errorf("connection/non-listen row leaked: %+v", l)
		}
	}
}

// lsof prints the name before the state, but parseLsof must not depend on
// the field order: state-first and name-first output give the same result,
// with and without "f" lines.
func TestParseLsofFieldOrder(t *testing.T) {
	want := []Listener{
		{3000, 10, "node", "*:3000"},
		{5000, 10, "node", "*:5000"},
	}
	orders := map[string]string{
		"name then state, no f lines":  "p10\ncnode\nn*:3000\nTST=LISTEN\nn*:4000\nTST=CLOSE_WAIT\nn*:5000\nTST=LISTEN\n",
		"state then name, no f lines":  "p10\ncnode\nTST=LISTEN\nn*:3000\nTST=CLOSE_WAIT\nn*:4000\nTST=LISTEN\nn*:5000\n",
		"name then state, with f":      "p10\ncnode\nf1\nn*:3000\nTST=LISTEN\nf2\nn*:4000\nTST=CLOSE_WAIT\nf3\nn*:5000\nTST=LISTEN\n",
		"state then name, with f":      "p10\ncnode\nf1\nTST=LISTEN\nn*:3000\nf2\nTST=CLOSE_WAIT\nn*:4000\nf3\nTST=LISTEN\nn*:5000\n",
		"state first for single entry": "p10\ncnode\nTST=LISTEN\nn*:3000\np11\ncx\nTST=CLOSE_WAIT\nn*:4000\np10\ncnode\nTST=LISTEN\nn*:5000\n",
	}
	for name, out := range orders {
		t.Run(name, func(t *testing.T) {
			if got := parseLsof(out); !reflect.DeepEqual(got, want) {
				t.Fatalf("got %+v want %+v", got, want)
			}
		})
	}
}

func TestParseLsofCRLFAndGarbage(t *testing.T) {
	got := parseLsof("p10\r\ncnode\r\nn*:3000\r\nTST=LISTEN\r\n\r\nxjunk\nn\np\nnnotaport\n")
	want := []Listener{{3000, 10, "node", "*:3000"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestFilterExactPort(t *testing.T) {
	ls := parseLsof(lsofEntry(1000, "a", "*:3000") + lsofEntry(1001, "b", "*:13000") + lsofEntry(1002, "c", "*:30001") + lsofEntry(1003, "d", "*:300"))
	got := groupByPort(normalizeListeners(ls))[3000]
	if len(got) != 1 || got[0].PID != 1000 {
		t.Fatalf("port 3000 matched %+v", got)
	}
}

func TestParseSS(t *testing.T) {
	out := "" +
		"State  Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n" +
		`LISTEN 0 511 0.0.0.0:3000 0.0.0.0:* users:(("node",pid=1234,fd=19),("node",pid=1235,fd=19))` + "\n" +
		`LISTEN 0 4096 127.0.0.53%lo:53 0.0.0.0:* users:(("systemd-resolve",pid=800,fd=14))` + "\n" +
		`LISTEN 0 128 [::]:8080 [::]:* users:(("java",pid=77,fd=3))` + "\n" +
		`LISTEN 0 128 [::ffff:127.0.0.1]:8081 [::]:* users:(("my app",pid=78,fd=3))` + "\n" +
		"LISTEN 0 128 *:9000 *:*\n" +
		`ESTAB 0 0 127.0.0.1:3000 127.0.0.1:5000 users:(("x",pid=9,fd=1))` + "\n"
	got := parseSS(out)
	want := []Listener{
		{3000, 1234, "node", "0.0.0.0:3000"},
		{3000, 1235, "node", "0.0.0.0:3000"},
		{53, 800, "systemd-resolve", "127.0.0.53%lo:53"},
		{8080, 77, "java", "[::]:8080"},
		{8081, 78, "my app", "[::ffff:127.0.0.1]:8081"},
		{9000, 0, "", "*:9000"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestParseNetstat(t *testing.T) {
	out := netstatHeader +
		"  TCP    0.0.0.0:3000           0.0.0.0:0              LISTENING       1234\r\r\n" +
		"  TCP    [::]:3000              [::]:0                 LISTENING       1234\r\r\n" +
		"  TCP    127.0.0.1:3000         127.0.0.1:50000        ESTABLISHED     555\n" +
		"  TCP    127.0.0.1:50001        127.0.0.1:3000         ESTABLISHED     556\n" +
		"  TCP    127.0.0.1:3000         10.0.0.1:443           TIME_WAIT       0\n" +
		"  TCP    127.0.0.1:3000         10.0.0.1:443           CLOSE_WAIT      557\n" +
		"  UDP    0.0.0.0:3000           *:*                                    777\n" +
		"  UDP    [::]:3000              *:*                                    778\n" +
		"  TCP    0.0.0.0:3001           0.0.0.0:0              ABHÖREN         888\n" + // localized state
		"  TCP    [::]:3003              [::]:0                 ÉCOUTE          889\n" +
		"  TCP    0.0.0.0:3002           0.0.0.0:0              LISTENING       0\n" +
		"  tcp    0.0.0.0:3004           0.0.0.0:0              listening       890\n"
	got := normalizeListeners(parseNetstat(out))
	want := []Listener{
		{3000, 1234, "", "0.0.0.0:3000"},
		{3001, 888, "", "0.0.0.0:3001"},
		{3003, 889, "", "[::]:3003"},
		{3004, 890, "", "0.0.0.0:3004"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

// A BOUND row has a wildcard foreign address and port 0 but is not listening.
func TestParseNetstatNonListeningStatesWithWildcardForeign(t *testing.T) {
	var b strings.Builder
	b.WriteString(netstatHeader)
	for i, st := range []string{"BOUND", "CLOSED", "SYN_SENT", "SYN_RECEIVED", "ESTABLISHED", "TIME_WAIT", "CLOSE_WAIT", "FIN_WAIT_1", "FIN_WAIT_2", "LAST_ACK", "CLOSING", "DELETE_TCB", "bound"} {
		b.WriteString("  TCP    127.0.0.1:" + strconv.Itoa(3000+i) + "         0.0.0.0:0              " + st + "     55\n")
	}
	b.WriteString("  TCP    127.0.0.1:3000         0.0.0.0:0              BOUND           55\n")
	if got := parseNetstat(b.String()); len(got) != 0 {
		t.Fatalf("non-listening rows accepted: %+v", got)
	}
	// Localized LISTENING names (wildcard foreign, unknown state) still count.
	got := parseNetstat(netstatHeader + "  TCP    0.0.0.0:3000           0.0.0.0:0              ABHÖREN         7\n")
	if len(got) != 1 || got[0].Port != 3000 {
		t.Fatalf("localized listener lost: %+v", got)
	}
}

func TestParseTasklist(t *testing.T) {
	out := "\"System\",\"4\",\"Services\",\"0\",\"144 K\"\r\r\n" +
		"\"node.exe\",\"1234\",\"Console\",\"1\",\"50,000 K\"\r\r\n" +
		"INFO: No tasks are running which match the specified criteria.\r\r\n" +
		"\"bad\",\"notapid\",\"x\"\n\n"
	got := parseTasklist(out)
	want := map[int]string{4: "System", 1234: "node.exe"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestParseDockerPorts(t *testing.T) {
	tests := []struct {
		name  string
		field string
		want  []int
	}{
		{"v4+v6 dup", "0.0.0.0:8080->80/tcp, [::]:8080->80/tcp", []int{8080}},
		{"triple colon v6", ":::8080->80/tcp", []int{8080}},
		{"loopback", "127.0.0.1:5432->5432/tcp", []int{5432}},
		{"compact range", "0.0.0.0:8000-8002->8000-8002/tcp", []int{8000, 8001, 8002}},
		{"separate mappings", "127.0.0.1:47810->8000/tcp, 127.0.0.1:47811->8001/tcp", []int{47810, 47811}},
		{"udp ignored", "0.0.0.0:53->53/udp", nil},
		{"unpublished ignored", "80/tcp, 443/tcp", nil},
		{"mixed", "127.0.0.1:47820->53/udp, 0.0.0.0:47801->80/tcp, [::]:47801->80/tcp, 53/udp", []int{47801}},
		{"empty", "", nil},
		{"reversed range ignored", "0.0.0.0:8002-8000->80/tcp", nil},
		{"garbage", "->/tcp, abc->80/tcp", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseDockerPorts(tt.field)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestParseDockerPS(t *testing.T) {
	out := "abc123|web|0.0.0.0:3000->80/tcp\n" +
		"def456|api|0.0.0.0:3000->80/tcp, 0.0.0.0:3001->81/tcp\n" +
		"badline\n" +
		"ghi789|idle|\n"
	got := parseDockerPS(out)
	if len(got[3000]) != 2 || got[3000][0].Name != "api" || got[3000][1].Name != "web" {
		t.Fatalf("3000: %+v", got[3000])
	}
	if len(got[3001]) != 1 || got[3001][0].ID != "def456" {
		t.Fatalf("3001: %+v", got[3001])
	}
	if len(got) != 2 {
		t.Fatalf("unexpected ports: %+v", got)
	}
}

func TestClassifyProcess(t *testing.T) {
	tests := []struct {
		name   string
		docker bool
		want   string
	}{
		{"node", false, catRuntime},
		{"Node.exe", false, catRuntime},
		{"python3.12", false, catRuntime},
		{"python3", false, catRuntime},
		{"go", false, catRuntime},
		{"java", false, catRuntime},
		{"language_server_macos_arm", false, catRuntime},
		{"next-server", false, catRuntime},
		{"next-server (v14.2.3)", false, catRuntime},
		{"next-router-worker", false, catRuntime},
		{"systemd-journald", false, catSystem},
		{"systemd-timesyncd", false, catSystem},
		{"systemd-foo", false, catSystem},
		{"systemdemo", false, catApplication},    // prefix needs the dash
		{"google chrome", false, catApplication}, // not "go"
		{"Google Chrome Helper", false, catApplication},
		{"mongod", false, catApplication}, // not "go"
		{"nextcloud", false, catApplication},
		{"initializer", false, catApplication}, // not "init"
		{"cronjob-runner", false, catApplication},
		{"postgres", false, catApplication},
		{"systemd", false, catSystem},
		{"systemd-resolved", false, catSystem},
		{"svchost.exe", false, catSystem},
		{"System", false, catSystem},
		{"kworker/0:1", false, catSystem},
		{"launchd", false, catSystem},
		{"com.docker.backend", false, catSystem},
		{"docker-proxy", false, catSystem},
		{"vpnkit-bridge", false, catSystem},
		{"", false, catUnknown},
		{"   ", false, catUnknown},
		{"anything", true, catApplication},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyProcess(tt.name, tt.docker); got != tt.want {
				t.Fatalf("classifyProcess(%q) = %s, want %s", tt.name, got, tt.want)
			}
		})
	}
}

func TestSafeToKill(t *testing.T) {
	mk := func(pid, name string, docker bool) CheckResult {
		r := CheckResult{PID: pid, ProcessName: name, IsDocker: docker, Category: classifyProcess(name, docker)}
		return r
	}
	tests := []struct {
		name string
		r    CheckResult
		want bool
	}{
		{"node", mk("100", "node", false), true},
		{"app", mk("100", "postgres", false), true},
		{"docker", mk("", "[Docker] web", true), true},
		{"system", mk("100", "svchost.exe", false), false},
		{"pid 1", mk("1", "node", false), false},
		{"pid 0", mk("", "node", false), false},
		{"unknown name", mk("100", "", false), false},
		{"own pid", mk("4242", "node", false), false},
		{"docker infra", mk("100", "com.docker.backend", false), false},
		{"free", CheckResult{Port: "1", NoListener: true}, false},
		{"error", CheckResult{Port: "1", Error: "x", ProcessName: "node", PID: "5", Category: catRuntime}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeToKill(tt.r, fakeOwnPID); got != tt.want {
				t.Fatalf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestIsDockerInfra(t *testing.T) {
	for _, n := range []string{"com.docker.backend", "docker", "dockerd", "docker-proxy", "vpnkit", "vpnkit-bridge", "containerd", "containerd-shim", "rootlesskit", "Docker.exe", "COM.DOCKER.VMNETD"} {
		if !isDockerInfra(n) {
			t.Errorf("%q should be infra", n)
		}
	}
	for _, n := range []string{"", "node", "dockerize", "my-docker-app", "containers"} {
		if isDockerInfra(n) {
			t.Errorf("%q should not be infra", n)
		}
	}
}
