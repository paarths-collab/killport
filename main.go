// killport finds and stops the processes (or Docker containers) holding TCP
// ports. It only considers TCP sockets in the LISTEN state, matched by their
// exact local port; UDP sockets and established connections are ignored.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// Version is injected by goreleaser (-X main.Version=...). Source builds keep
// "dev" unless the module version is available from build info.
var Version = "dev"

func version() string {
	if Version != "dev" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := bi.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return Version
}

type options struct {
	verbose bool
	dryRun  bool
	json    bool
	check   bool
}

type app struct {
	sys    system
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	opts   options
	// workflow is set when an inspect / plan / apply subcommand runs (or
	// failed to parse): its fatal JSON errors also carry an exit_class.
	workflow bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, newOSSystem()))
}

// wantsJSON scans raw arguments so even flag-parse errors can honour --json.
// It follows pflag's rules: --json, --json=<bool>, and the shorthand -j
// anywhere in a bundle of short flags (-cj). In a bundle a "=value" belongs
// to the letter right before it, so "-d=j" does not enable JSON. The last
// occurrence wins and scanning stops at "--".
func wantsJSON(args []string) bool {
	on := false
	for _, arg := range args {
		switch {
		case arg == "--":
			return on
		case arg == "--json":
			on = true
		case strings.HasPrefix(arg, "--json="):
			on = parseBoolDefaultTrue(strings.TrimPrefix(arg, "--json="))
		case len(arg) > 1 && arg[0] == '-' && arg[1] != '-':
			letters, val, hasVal := strings.Cut(arg[1:], "=")
			for i := 0; i < len(letters); i++ {
				if letters[i] != 'j' {
					continue
				}
				if hasVal && i == len(letters)-1 {
					on = parseBoolDefaultTrue(val)
				} else {
					on = true
				}
			}
		}
	}
	return on
}

// parseBoolDefaultTrue parses a flag value; anything unparsable counts as
// true so a malformed "--json=maybe" still gets its usage error as JSON.
func parseBoolDefaultTrue(v string) bool {
	b, err := strconv.ParseBool(v)
	return err != nil || b
}

// run executes killport and returns the process exit code:
// 0 success, 1 runtime failure, 2 usage error.
func run(args []string, stdout, stderr io.Writer, sys system) int {
	return runIO(args, os.Stdin, stdout, stderr, sys)
}

// runIO is run with an explicit standard input (used by `apply -`).
func runIO(args []string, stdin io.Reader, stdout, stderr io.Writer, sys system) int {
	a := &app{stdin: stdin, stdout: stdout, stderr: stderr}
	exit := 0

	cmd := &cobra.Command{
		Use:           "killport [port...]",
		Version:       version(),
		Short:         "Free up local ports by killing the processes (or Docker containers) attached to them.",
		Long:          "Free up local ports by stopping the processes (or Docker containers) that LISTEN on them.\nOnly TCP listeners are considered; UDP sockets and client connections are never touched.",
		Example:       "killport 3000\nkillport 3000-3005\nkillport 8080 --json\nkillport 3000 --dry-run\nkillport --check\nkillport --check 3000 8080\nkillport plan 3000 -o plan.json && killport apply plan.json",
		SilenceErrors: true,
		SilenceUsage:  true,
		// Ports are positional arguments; with subcommands present cobra would
		// otherwise reject "killport 3000" as an unknown command.
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, cmdArgs []string) error {
			code, err := a.execute(cmd.Context(), cmdArgs)
			exit = code
			return err
		},
	}
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.Flags().BoolVarP(&a.opts.verbose, "verbose", "v", false, "Log every external command and its duration to stderr")
	cmd.Flags().BoolVarP(&a.opts.dryRun, "dry-run", "d", false, "Show what would be killed or stopped without doing it")
	cmd.Flags().BoolVarP(&a.opts.json, "json", "j", false, "Output results as machine-readable JSON (for AI agents)")
	cmd.Flags().BoolVarP(&a.opts.check, "check", "c", false, "Check which process is using port(s) without killing")

	a.sys = sys
	cmd.CompletionOptions.DisableDefaultCmd = true
	a.addWorkflowCommands(cmd, &exit)

	// With --json stdout must stay JSON even for --help and --version, which
	// cobra would otherwise print as plain text.
	cmd.InitDefaultHelpFlag()
	cmd.InitDefaultVersionFlag()
	if sub, subArgs, ferr := cmd.Find(args); ferr == nil && sub != cmd {
		// A workflow subcommand: its own --help, as JSON when asked.
		a.workflow = true
		sub.InitDefaultHelpFlag()
		if perr := sub.ParseFlags(subArgs); perr == nil && a.opts.json {
			if helpFlag, _ := sub.Flags().GetBool("help"); helpFlag {
				var usage bytes.Buffer
				sub.SetOut(&usage)
				_ = sub.Help()
				sub.SetOut(stdout)
				writeJSON(stdout, map[string]string{"usage": usage.String()})
				return 0
			}
		}
	} else if perr := cmd.ParseFlags(args); perr == nil && a.opts.json {
		helpFlag, _ := cmd.Flags().GetBool("help")
		versionFlag, _ := cmd.Flags().GetBool("version")
		switch {
		case helpFlag:
			var usage bytes.Buffer
			cmd.SetOut(&usage)
			_ = cmd.Help()
			cmd.SetOut(stdout)
			writeJSON(stdout, map[string]string{"usage": usage.String()})
			return 0
		case versionFlag:
			writeJSON(stdout, map[string]string{"version": version()})
			return 0
		}
	}

	if err := cmd.Execute(); err != nil {
		var ce *cliError
		if !errors.As(err, &ce) {
			// Flag-parse failures from cobra/pflag are usage errors.
			ce = usageErr(err.Error())
			a.opts.json = a.opts.json || wantsJSON(args)
		}
		a.reportFatal(ce)
		return ce.Exit
	}
	return exit
}

// execute is the body of the command. It returns the exit code for a normal
// completion, or a *cliError for fatal problems.
func (a *app) execute(ctx context.Context, args []string) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if a.opts.verbose {
		a.sys = newVerboseSystem(a.sys, a.stderr)
	}
	if a.opts.dryRun {
		// Defence in depth: a dry run must never be able to mutate anything.
		a.sys = readOnlySystem{system: a.sys}
	}

	var ports []int
	if len(args) > 0 {
		var err error
		ports, err = parsePortSpecs(args)
		if err != nil {
			return 2, usageErr(err.Error())
		}
	} else if !a.opts.check {
		return 2, usageErr("requires at least 1 port (or use --check to inspect)")
	}

	if missing := missingTools(a.sys); len(missing) > 0 {
		return 1, &cliError{Code: "missing_dependency", Exit: 1, Msg: "missing required system tools: " + strings.Join(missing, ", ")}
	}

	lst, err := listListeners(ctx, a.sys)
	if err != nil {
		return 1, &cliError{Code: "enumeration_failed", Exit: 1, Msg: fmt.Sprintf("could not list listening ports: %v", err)}
	}
	for _, w := range lst.Warnings {
		a.warn(w)
	}
	dk := queryDocker(ctx, a.sys)
	if dk.Warning != "" {
		a.warn(dk.Warning)
	}

	if a.opts.check {
		rows := buildCheckRows(ports, lst.Listeners, dk, a.sys.Getpid())
		a.printCheck(rows, len(ports) > 0)
		return 0, nil
	}

	rows, failed := a.runKill(ctx, ports, lst, dk)
	a.printKill(rows)
	if failed {
		return 1, nil
	}
	return 0, nil
}
