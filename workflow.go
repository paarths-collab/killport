package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// This file wires the opt-in structured workflow (inspect / plan / apply) into
// the CLI and renders its human and JSON output. The legacy
// `killport <ports>` command is untouched.

const (
	inspectKind   = "killport.inspect"
	maxPlanBytes  = 16 << 20
	protocolUsage = "Protocol: tcp (listening sockets, default) or udp (bound sockets, matched by LOCAL port)"
)

// addWorkflowCommands registers the subcommands on the root command. exit
// receives the exit code of whichever subcommand runs.
func (a *app) addWorkflowCommands(root *cobra.Command, exit *int) {
	root.AddCommand(a.inspectCmd(exit), a.planCmd(exit), a.applyCmd(exit))
}

func (a *app) commonFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&a.opts.verbose, "verbose", "v", false, "Log every external command and its duration to stderr")
	cmd.Flags().BoolVarP(&a.opts.json, "json", "j", false, "Output one machine-readable JSON document")
}

func (a *app) inspectCmd(exit *int) *cobra.Command {
	var protocol string
	cmd := &cobra.Command{
		Use:           "inspect [port...]",
		Short:         "Observe who occupies ports, and how completely that can be seen (read-only)",
		Example:       "killport inspect 3000\nkillport inspect 3000-3005 --json\nkillport inspect --protocol udp 5353",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a.workflow = true
			code, err := a.runInspect(cmd.Context(), args, protocol)
			*exit = code
			return err
		},
	}
	cmd.Flags().StringVar(&protocol, "protocol", protoTCP, protocolUsage)
	a.commonFlags(cmd)
	return cmd
}

func (a *app) planCmd(exit *int) *cobra.Command {
	opts := planOptions{}
	var output string
	cmd := &cobra.Command{
		Use:           "plan <port...>",
		Short:         "Write a plan of what would be stopped, bound to process identities (never changes anything)",
		Example:       "killport plan 3000 -o plan.json\nkillport plan 3000 3001 --force --json\nkillport plan --protocol udp 5353",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a.workflow = true
			code, err := a.runPlan(cmd.Context(), args, opts, output)
			*exit = code
			return err
		},
	}
	cmd.Flags().StringVar(&opts.Protocol, "protocol", protoTCP, protocolUsage)
	cmd.Flags().BoolVar(&opts.Force, "force", false, "Plan SIGKILL instead of SIGTERM (Windows always terminates)")
	cmd.Flags().DurationVar(&opts.TTL, "ttl", defaultPlanTTL, "How long the plan stays valid (max 24h)")
	cmd.Flags().DurationVar(&opts.Grace, "grace", defaultGrace, "Docker stop grace period and how long apply waits for the endpoint to become free")
	cmd.Flags().StringVarP(&output, "output", "o", "", "Write the plan JSON to this file")
	a.commonFlags(cmd)
	return cmd
}

func (a *app) applyCmd(exit *int) *cobra.Command {
	cmd := &cobra.Command{
		Use:           "apply <plan.json | ->",
		Short:         "Carry out a plan, re-validating every process and container identity first",
		Example:       "killport apply plan.json\nkillport plan 3000 --json | killport apply -",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a.workflow = true
			code, err := a.runApply(cmd.Context(), args)
			*exit = code
			return err
		},
	}
	a.commonFlags(cmd)
	return cmd
}

// workflowSystem layers verbose logging and, for read-only commands, the
// readOnlySystem guard over the real system.
func (a *app) workflowSystem(readOnly bool) system {
	sys := a.sys
	if a.opts.verbose {
		sys = newVerboseSystem(sys, a.stderr)
	}
	if readOnly {
		sys = readOnlySystem{system: sys}
	}
	return sys
}

func parseProtocol(p string) (string, *cliError) {
	switch p {
	case protoTCP, protoUDP:
		return p, nil
	}
	return "", usageErr(fmt.Sprintf("invalid --protocol %q: expected tcp or udp", p))
}

func (a *app) requireTools(sys system) *cliError {
	if missing := missingTools(sys); len(missing) > 0 {
		return &cliError{Code: "missing_dependency", Exit: exitFailed, Msg: "missing required system tools: " + strings.Join(missing, ", ")}
	}
	return nil
}

func (a *app) warnAll(ws []string) {
	for _, w := range ws {
		a.warn(w)
	}
}

// ---- inspect ---------------------------------------------------------------

type inspectDoc struct {
	SchemaVersion int               `json:"schema_version"`
	Kind          string            `json:"kind"`
	ToolVersion   string            `json:"tool_version"`
	OS            string            `json:"os"`
	ObservedAt    string            `json:"observed_at"`
	Request       inspectRequest    `json:"request"`
	Endpoints     []inspectEndpoint `json:"endpoints"`
	Warnings      []string          `json:"warnings"`
	ExitClass     string            `json:"exit_class"`
}

type inspectRequest struct {
	Protocol string `json:"protocol"`
	Scope    string `json:"scope"`
	Ports    []int  `json:"ports"`
	All      bool   `json:"all"`
}

type inspectEndpoint struct {
	Protocol     string             `json:"protocol"`
	Port         int                `json:"port"`
	Visibility   string             `json:"visibility"`
	Occupied     bool               `json:"occupied"`
	Unattributed bool               `json:"unattributed"`
	Owners       []inspectOwner     `json:"owners"`
	Containers   []inspectContainer `json:"containers"`
}

type inspectOwner struct {
	PID              int    `json:"pid"`
	Name             string `json:"name"`
	StartTime        string `json:"start_time,omitempty"`
	IdentityStrength string `json:"identity_strength,omitempty"`
}

type inspectContainer struct {
	ContainerID string `json:"container_id"`
	Name        string `json:"name"`
}

func (a *app) runInspect(ctx context.Context, args []string, protocolFlag string) (int, error) {
	protocol, perr := parseProtocol(protocolFlag)
	if perr != nil {
		return exitUsage, perr
	}
	var ports []int
	if len(args) > 0 {
		var err error
		if ports, err = parsePortSpecs(args); err != nil {
			return exitUsage, usageErr(err.Error())
		}
	}
	sys := a.workflowSystem(true)
	if cerr := a.requireTools(sys); cerr != nil {
		return cerr.Exit, cerr
	}
	snap := collect(ctx, sys, protocol)
	a.warnAll(snap.warnings)

	eps := snap.endpoints(ports)
	doc := inspectDoc{
		SchemaVersion: planSchemaVersion, Kind: inspectKind, ToolVersion: version(), OS: sys.GOOS(),
		ObservedAt: formatTime(sys.Now()),
		Request:    inspectRequest{Protocol: protocol, Scope: scopeFor(protocol), Ports: append([]int{}, ports...), All: ports == nil},
		Endpoints:  []inspectEndpoint{},
		Warnings:   append([]string{}, snap.warnings...),
	}
	code := exitOK
	if snap.providerFailed() {
		code = exitFailed
	}
	if ports == nil && !snap.tableOK {
		code = worseExit(code, exitUncertain)
	}
	for _, e := range eps {
		ie := inspectEndpoint{
			Protocol: e.Protocol, Port: e.Port, Visibility: e.Visibility, Occupied: e.Occupied, Unattributed: e.Unattributed,
			Owners: []inspectOwner{}, Containers: []inspectContainer{},
		}
		for _, o := range e.Owners {
			ie.Owners = append(ie.Owners, inspectOwner{PID: o.PID, Name: o.Name, StartTime: o.StartTime, IdentityStrength: o.Strength})
		}
		for _, c := range e.Containers {
			ie.Containers = append(ie.Containers, inspectContainer{ContainerID: c.ID, Name: c.Name})
		}
		doc.Endpoints = append(doc.Endpoints, ie)
		switch e.Visibility {
		case visProviderUnavailable:
			code = worseExit(code, exitFailed)
		case visPermissionLimited, visNoneVisible:
			code = worseExit(code, exitUncertain)
		}
	}
	doc.ExitClass = exitClassName(code)
	if a.opts.json {
		writeJSON(a.stdout, doc)
		return code, nil
	}
	a.renderInspect(doc)
	return code, nil
}

func (a *app) renderInspect(doc inspectDoc) {
	w := a.stdout
	scope := "all occupied ports"
	if !doc.Request.All {
		scope = "ports " + joinInts(doc.Request.Ports)
	}
	fmt.Fprintf(w, "[%s] %s %s on %s: %s\n", infoMark(), doc.Request.Protocol, doc.Request.Scope, doc.OS, scope)
	if len(doc.Endpoints) == 0 {
		fmt.Fprintf(w, "No occupied endpoints were found.\n")
	}
	for _, e := range doc.Endpoints {
		state := "free"
		if e.Occupied {
			state = "occupied"
		}
		fmt.Fprintf(w, "%s %d: %s (visibility: %s)\n", e.Protocol, e.Port, state, e.Visibility)
		for _, o := range e.Owners {
			id := "no identity"
			if o.IdentityStrength != "" {
				id = "identity " + o.IdentityStrength
			}
			fmt.Fprintf(w, "  pid %d (%s)  %s\n", o.PID, orUnknown(o.Name), id)
		}
		for _, c := range e.Containers {
			fmt.Fprintf(w, "  container %s  %s\n", c.Name, c.ContainerID)
		}
		if e.Unattributed {
			fmt.Fprintf(w, "  an occupying socket has no attributable owner (try sudo / Administrator)\n")
		}
	}
	fmt.Fprintf(w, "Result: %s\n", doc.ExitClass)
}

// ---- plan ------------------------------------------------------------------

func (a *app) runPlan(ctx context.Context, args []string, opts planOptions, output string) (int, error) {
	protocol, perr := parseProtocol(opts.Protocol)
	if perr != nil {
		return exitUsage, perr
	}
	opts.Protocol = protocol
	if len(args) == 0 {
		return exitUsage, usageErr("plan requires at least 1 port")
	}
	ports, err := parsePortSpecs(args)
	if err != nil {
		return exitUsage, usageErr(err.Error())
	}
	opts.Ports = ports
	if opts.TTL <= 0 || opts.TTL > maxPlanTTL {
		return exitUsage, usageErr(fmt.Sprintf("invalid --ttl %s: must be greater than 0 and at most %s", opts.TTL, maxPlanTTL))
	}
	if opts.Grace < 0 || opts.Grace > maxGrace {
		return exitUsage, usageErr(fmt.Sprintf("invalid --grace %s: must be between 0 and %s", opts.Grace, maxGrace))
	}

	// plan observes and writes a document; it must never be able to mutate.
	sys := a.workflowSystem(true)
	if cerr := a.requireTools(sys); cerr != nil {
		return cerr.Exit, cerr
	}
	snap := collect(ctx, sys, protocol)
	a.warnAll(snap.warnings)
	doc := buildPlan(sys, snap, opts, sys.Now())
	code := planClass(doc)

	if output != "" {
		data, merr := json.MarshalIndent(doc, "", "  ")
		if merr == nil {
			merr = os.WriteFile(output, append(data, '\n'), 0o644)
		}
		if merr != nil {
			return exitFailed, &cliError{Code: "write_failed", Exit: exitFailed, Msg: fmt.Sprintf("could not write the plan: %v", merr)}
		}
	}
	if a.opts.json {
		writeJSON(a.stdout, doc)
		return code, nil
	}
	a.renderPlan(doc, output)
	return code, nil
}

func (a *app) renderPlan(doc *planDoc, output string) {
	w := a.stdout
	fmt.Fprintf(w, "[%s] Plan %s\n", infoMark(), doc.PlanID)
	fmt.Fprintf(w, "%s %s ports %s on %s; created %s, expires %s\n",
		doc.Request.Protocol, doc.Request.Scope, joinInts(doc.Request.Ports), doc.OS, doc.CreatedAt, doc.ExpiresAt)
	fmt.Fprintf(w, "Resources (%d):\n", len(doc.Resources))
	for _, r := range doc.Resources {
		fmt.Fprintf(w, "  %s\n", resourceLine(r))
	}
	fmt.Fprintf(w, "Refused (%d):\n", len(doc.Refused))
	for _, r := range doc.Refused {
		fmt.Fprintf(w, "  %s\n", refusalLine(r))
	}
	fmt.Fprintf(w, "Endpoints (%d):\n", len(doc.Endpoints))
	for _, e := range doc.Endpoints {
		state := "free"
		if e.Occupied {
			state = "occupied"
		}
		fmt.Fprintf(w, "  %s %d: %s, visibility %s, resources [%s]\n", e.Protocol, e.Port, state, e.Visibility, strings.Join(e.Resources, " "))
	}
	fmt.Fprintf(w, "Nothing was changed (actions_executed: %d). Result: %s\n", doc.ActionsExecuted, doc.ExitClass)
	if output != "" {
		fmt.Fprintf(w, "[%s] Plan written to %s; review it, then run: killport apply %s\n", okMark(), output, output)
	} else {
		fmt.Fprintf(w, "[%s] Add -o plan.json to save this plan, then run: killport apply plan.json\n", infoMark())
	}
}

// ---- apply -----------------------------------------------------------------

func (a *app) readPlan(arg string) ([]byte, error) {
	var (
		data []byte
		err  error
	)
	if arg == "-" {
		if a.stdin == nil {
			return nil, fmt.Errorf("no standard input available")
		}
		data, err = io.ReadAll(io.LimitReader(a.stdin, maxPlanBytes+1))
	} else {
		var f *os.File
		if f, err = os.Open(arg); err == nil {
			defer f.Close()
			data, err = io.ReadAll(io.LimitReader(f, maxPlanBytes+1))
		}
	}
	if err != nil {
		return nil, err
	}
	if len(data) > maxPlanBytes {
		return nil, fmt.Errorf("plan is larger than %d bytes", maxPlanBytes)
	}
	return data, nil
}

func (a *app) runApply(ctx context.Context, args []string) (int, error) {
	if len(args) != 1 {
		return exitUsage, usageErr("apply requires exactly one argument: a plan file, or - for standard input")
	}
	data, err := a.readPlan(args[0])
	if err != nil {
		return exitUsage, usageErr(fmt.Sprintf("cannot read the plan: %v", err))
	}
	doc, err := decodePlan(data)
	if err != nil {
		return exitUsage, usageErr(err.Error())
	}
	if doc.OS != a.sys.GOOS() {
		return exitUsage, usageErr(fmt.Sprintf("the plan was created on %s and cannot be applied on %s; make a new plan here", doc.OS, a.sys.GOOS()))
	}
	if err := validatePlan(doc); err != nil {
		return exitUsage, usageErr(err.Error())
	}

	sys := a.workflowSystem(false)
	now := sys.Now()
	expires, _ := time.Parse(time.RFC3339, doc.ExpiresAt)
	if now.After(expires) {
		res := expiredApplyDoc(doc, now)
		a.printApply(res)
		return exitFreshPlan, nil
	}
	if cerr := a.requireTools(sys); cerr != nil {
		return cerr.Exit, cerr
	}
	res, code, warnings := applyPlan(ctx, sys, doc)
	a.warnAll(warnings)
	a.printApply(res)
	return code, nil
}

func (a *app) printApply(res *applyDoc) {
	if a.opts.json {
		writeJSON(a.stdout, res)
		return
	}
	w := a.stdout
	fmt.Fprintf(w, "[%s] Apply plan %s\n", infoMark(), res.PlanID)
	if res.Error != "" {
		fmt.Fprintf(w, "[%s] %s; nothing was attempted. Make a fresh plan.\n", errMark(), res.Error)
	}
	fmt.Fprintf(w, "Resources (%d):\n", len(res.Resources))
	for _, r := range res.Resources {
		line := fmt.Sprintf("  %s  %s  verified_gone=%t forced=%t", r.ResourceID, r.Result, r.VerifiedGone, r.Forced)
		if r.Note != "" {
			line += "  (" + r.Note + ")"
		}
		if r.Error != "" {
			line += "  error: " + r.Error
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintf(w, "Endpoints (%d):\n", len(res.Endpoints))
	for _, e := range res.Endpoints {
		line := fmt.Sprintf("  %s %d: %s (visibility: %s)", e.Protocol, e.Port, e.Status, e.Visibility)
		for _, h := range e.Holders {
			if h.ContainerID != "" {
				line += fmt.Sprintf("; held by container %s (%s)", h.Name, h.ContainerID)
			} else {
				line += fmt.Sprintf("; held by pid %d (%s)", h.PID, orUnknown(h.Name))
			}
		}
		fmt.Fprintln(w, line)
	}
	fmt.Fprintf(w, "Actions executed: %d. Result: %s\n", res.ActionsExecuted, res.ExitClass)
}
