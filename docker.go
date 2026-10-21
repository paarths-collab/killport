package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

type container struct {
	ID   string
	Name string
}

// dockerState is the result of asking Docker which containers publish which
// host ports.
type dockerState struct {
	Available bool   // docker binary found on PATH
	Warning   string // non-empty when `docker ps` failed
	Owners    map[int][]container
}

const (
	dockerPSTimeout   = 15 * time.Second
	dockerStopTimeout = 45 * time.Second // docker's own stop grace is 10s
)

// queryDocker lists running containers. A missing docker binary is silent;
// a failing `docker ps` yields a warning and no owners.
func queryDocker(ctx context.Context, sys system) dockerState {
	st := dockerState{Owners: map[int][]container{}}
	if !hasTool(sys, "docker") {
		return st
	}
	st.Available = true
	out, err := runTool(ctx, sys, dockerPSTimeout, "docker", "ps", "--format", "{{.ID}}|{{.Names}}|{{.Ports}}")
	if err != nil {
		st.Warning = fmt.Sprintf("could not query Docker (%v); Docker-published ports may be missing from the results", err)
		return st
	}
	st.Owners = parseDockerPS(string(out))
	return st
}

// parseDockerPS parses `docker ps --format '{{.ID}}|{{.Names}}|{{.Ports}}'`.
func parseDockerPS(out string) map[int][]container {
	owners := make(map[int][]container)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 3 {
			continue
		}
		c := container{ID: strings.TrimSpace(parts[0]), Name: strings.TrimSpace(parts[1])}
		if c.ID == "" {
			continue
		}
		for _, p := range parseDockerPorts(parts[2]) {
			owners[p] = append(owners[p], c)
		}
	}
	for p, cs := range owners {
		sort.Slice(cs, func(i, j int) bool {
			if cs[i].Name != cs[j].Name {
				return cs[i].Name < cs[j].Name
			}
			return cs[i].ID < cs[j].ID
		})
		owners[p] = cs
	}
	return owners
}

// parseDockerPorts extracts the published host TCP ports from a docker ps
// "Ports" column, e.g. "0.0.0.0:8080->80/tcp, [::]:8080->80/tcp,
// 0.0.0.0:8000-8002->8000-8002/tcp, 53/udp". Host ranges are expanded; UDP
// and unpublished ports are ignored; the result is sorted and de-duplicated.
func parseDockerPorts(field string) []int { return parseDockerPortsProto(field, "tcp") }

// parseDockerPortsProto is parseDockerPorts for an explicit protocol ("tcp" or
// "udp"): only mappings ending in "/<protocol>" are considered.
func parseDockerPortsProto(field, protocol string) []int {
	seen := make(map[int]bool)
	for _, m := range strings.Split(field, ",") {
		m = strings.TrimSpace(m)
		idx := strings.Index(m, "->")
		if idx < 0 || !strings.HasSuffix(m, "/"+protocol) {
			continue
		}
		host := m[:idx]
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = host[i+1:]
		}
		if lo, hi, ok := strings.Cut(host, "-"); ok {
			a, err1 := parseDecimalPort(lo)
			b, err2 := parseDecimalPort(hi)
			if err1 != nil || err2 != nil || a > b {
				continue
			}
			for p := a; p <= b; p++ {
				seen[p] = true
			}
			continue
		}
		if p, err := parseDecimalPort(host); err == nil {
			seen[p] = true
		}
	}
	ports := make([]int, 0, len(seen))
	for p := range seen {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return ports
}

// normProcName lowercases a process name and strips a trailing ".exe" so
// names compare exactly.
func normProcName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.TrimSuffix(n, ".exe")
	return n
}

// isDockerInfra reports whether a native process is part of Docker's own
// plumbing (port forwarders, VM helpers), which must never be killed
// directly: the owning container has to be stopped instead.
func isDockerInfra(name string) bool {
	n := normProcName(name)
	if n == "" {
		return false
	}
	switch n {
	case "docker", "dockerd", "docker-proxy", "rootlesskit":
		return true
	}
	return strings.HasPrefix(n, "com.docker.") ||
		strings.HasPrefix(n, "vpnkit") ||
		strings.HasPrefix(n, "containerd")
}
