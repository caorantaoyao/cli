// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/larksuite/cli/errs"
	"github.com/larksuite/cli/extension/platform"
	"github.com/larksuite/cli/internal/cmdutil"
	"github.com/larksuite/cli/internal/output"
)

// writeAppConfigWithoutUser installs a config holding an app but no logged-in
// user, which is what drives `auth check` down its exit-code-only signal path.
func writeAppConfigWithoutUser(t *testing.T, cfgDir string) {
	t.Helper()
	body := `{"apps":[{"name":"probe","appId":"cli_probe","appSecret":"probe-secret","brand":"feishu","users":[]}],"currentApp":"probe"}`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// captureShutdownErr registers a plugin whose only job is to record the error
// handed to the Shutdown lifecycle event.
func captureShutdownErr(t *testing.T, observed *error, fired *int) {
	t.Helper()
	platform.ResetForTesting()
	t.Cleanup(platform.ResetForTesting)
	platform.Register(platform.NewPlugin("probe", "1.0").
		On(platform.Shutdown, "capture",
			func(_ context.Context, lc *platform.LifecycleContext) error {
				*fired++
				*observed = lc.Err
				return nil
			}).
		MustBuild())
}

func quietNotices(t *testing.T) {
	t.Helper()
	t.Setenv("LARKSUITE_CLI_NO_UPDATE_NOTIFIER", "1")
	t.Setenv("LARKSUITE_CLI_NO_SKILLS_NOTIFIER", "1")
}

// TestExitCodeOnlySignalSurvivesFullDispatch pins the contract that an
// exit-code-only signal keeps its exit code and writes nothing to stderr when
// it travels the whole way through ExecuteWithOptions. Classifying it instead
// would put a second, contradictory error envelope on stderr next to the
// result already on stdout, and replace exit 1 with the internal-fault code.
func TestExitCodeOnlySignalSurvivesFullDispatch(t *testing.T) {
	cfgDir := tmpHome(t)
	quietNotices(t)
	writeAppConfigWithoutUser(t, cfgDir)

	var observed error
	fired := 0
	captureShutdownErr(t, &observed, &fired)

	code, stdout, stderr := executeWithCapturedOS(t, nil,
		"auth", "check", "--scope", "im:message:send_as_bot")

	if stderr != "" {
		t.Errorf("stderr must stay empty for an exit-code-only signal, got %q", stderr)
	}
	if !strings.Contains(stdout, `"ok": false`) {
		t.Errorf("the result envelope belongs on stdout, got %q", stdout)
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (the signal's own code)", code)
	}
	if fired != 1 {
		t.Fatalf("Shutdown handler fired %d times, want 1", fired)
	}
	if _, ok := errs.ProblemOf(observed); ok {
		t.Errorf("Shutdown observed a classified error %v; an exit-code-only "+
			"signal carries no Problem and must pass through unchanged", observed)
	}
	var bare *output.BareError
	if !errors.As(observed, &bare) {
		t.Errorf("Shutdown observed %T, want the original *output.BareError", observed)
	}
}

// TestShutdownHookCannotRewriteUserVisibleFailure pins that what the user got
// is settled before the Shutdown event fires. Typed errors carry exported
// fields, so a handler reaching through errs.ProblemOf really can write to the
// error it is given; ordering is what makes that harmless.
func TestShutdownHookCannotRewriteUserVisibleFailure(t *testing.T) {
	tmpHome(t)
	quietNotices(t)
	platform.ResetForTesting()
	t.Cleanup(platform.ResetForTesting)

	fired := 0
	platform.Register(platform.NewPlugin("tamper", "1.0").
		On(platform.Shutdown, "rewrite",
			func(_ context.Context, lc *platform.LifecycleContext) error {
				fired++
				if p, ok := errs.ProblemOf(lc.Err); ok {
					p.Category = errs.CategoryNetwork
					p.Subtype = "rewritten_by_plugin"
					p.Message = "rewritten by plugin"
					p.Hint = "rewritten by plugin"
				}
				return nil
			}).
		MustBuild())

	code, _, stderr := executeWithCapturedOS(t, nil, "definitely-not-a-command")

	// Without this, a Shutdown handler that never runs would leave the
	// envelope untouched and pass the assertions below for the wrong reason.
	if fired != 1 {
		t.Fatalf("Shutdown handler fired %d times, want 1", fired)
	}

	var envelope struct {
		Error struct {
			Category errs.Category `json:"type"`
			Subtype  errs.Subtype  `json:"subtype"`
			Message  string        `json:"message"`
			Hint     string        `json:"hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
		t.Fatalf("stderr is not a JSON envelope: %v\n%s", err, stderr)
	}
	if envelope.Error.Category != errs.CategoryValidation ||
		envelope.Error.Subtype != errs.SubtypeInvalidArgument {
		t.Errorf("plugin rewrote the envelope: %s/%s",
			envelope.Error.Category, envelope.Error.Subtype)
	}
	if strings.Contains(envelope.Error.Message, "rewritten") ||
		strings.Contains(envelope.Error.Hint, "rewritten") {
		t.Errorf("plugin rewrote user-visible text: message=%q hint=%q",
			envelope.Error.Message, envelope.Error.Hint)
	}
	if code != output.ExitValidation {
		t.Errorf("exit code = %d, want %d; a Shutdown handler must not change it",
			code, output.ExitValidation)
	}
}

// TestCobraValidationFailuresAreUserErrors covers every shape cobra rejects a
// command line with before any command body runs. All of them are mistakes in
// what the user typed, so none may be reported as an internal fault — that
// would tell the user the tool broke and would count their typo against
// service health.
func TestCobraValidationFailuresAreUserErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown command", []string{"definitely-not-a-command"}},
		{"unknown subcommand", []string{"sheets", "+definitely-nope"}},
		{"missing required flag", []string{"auth", "check"}},
		{"wrong argument count", []string{"profile", "remove", "first", "second"}},
		{"positional arg on a shortcut", []string{"wiki", "+space-list", "stray"}},
		{"flag group one-required", []string{
			"sheets", "+csv-put", "--spreadsheet-token", "Xxxxxxxxxxx",
			"--sheet-id", "abc", "--csv", "a,b"}},
		{"flag group mutually exclusive", []string{
			"sheets", "+csv-put", "--spreadsheet-token", "Xxxxxxxxxxx",
			"--sheet-id", "abc", "--csv", "a,b", "--start-cell", "A1", "--range", "A1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpHome(t)
			quietNotices(t)

			var observed error
			fired := 0
			captureShutdownErr(t, &observed, &fired)

			code, _, stderr := executeWithCapturedOS(t, nil, tc.args...)

			var envelope struct {
				Error struct {
					Category errs.Category `json:"type"`
					Subtype  errs.Subtype  `json:"subtype"`
					Message  string        `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
				t.Fatalf("stderr is not a JSON envelope: %v\n%s", err, stderr)
			}
			if envelope.Error.Category != errs.CategoryValidation ||
				envelope.Error.Subtype != errs.SubtypeInvalidArgument {
				t.Errorf("reported as %s/%s, want %s/%s",
					envelope.Error.Category, envelope.Error.Subtype,
					errs.CategoryValidation, errs.SubtypeInvalidArgument)
			}
			if code != output.ExitValidation {
				t.Errorf("exit code = %d, want %d", code, output.ExitValidation)
			}

			// The Shutdown handler must agree with what the user was told.
			if fired != 1 {
				t.Fatalf("Shutdown handler fired %d times, want 1", fired)
			}
			problem, ok := errs.ProblemOf(observed)
			if !ok {
				t.Fatalf("Shutdown observed unclassified %T (%v)", observed, observed)
			}
			if problem.Category != envelope.Error.Category ||
				problem.Subtype != envelope.Error.Subtype ||
				problem.Message != envelope.Error.Message {
				t.Errorf("Shutdown observed %s/%s %q, envelope reports %s/%s %q",
					problem.Category, problem.Subtype, problem.Message,
					envelope.Error.Category, envelope.Error.Subtype, envelope.Error.Message)
			}
			if got := output.ExitCodeOf(observed); got != code {
				t.Errorf("Shutdown observed exit code %d, process exited %d", got, code)
			}
		})
	}
}

// TestEveryArgsValidatorProducesTypedErrors is the guard that keeps a newly
// added positional validator from silently reporting a user's mistake as an
// internal fault: instrumentErrorStages must reach every command in the tree,
// so no Args validator can return an unclassified error.
func TestEveryArgsValidatorProducesTypedErrors(t *testing.T) {
	tmpHome(t)
	platform.ResetForTesting()
	t.Cleanup(platform.ResetForTesting)

	_, root, _ := buildInternal(context.Background(), buildInvocationForTest(t), WithoutPlugins())

	checked := 0
	var unguarded []string
	forEachCommand(root, func(c *cobra.Command) {
		if c.Args == nil {
			return
		}
		// Feed enough positional words that any bounded validator rejects them.
		err := c.Args(c, []string{"stray1", "stray2", "stray3", "stray4", "stray5",
			"stray6", "stray7", "stray8", "stray9", "stray10"})
		if err == nil {
			return
		}
		checked++
		problem, ok := errs.ProblemOf(err)
		if !ok {
			unguarded = append(unguarded, c.CommandPath()+" (unclassified)")
			return
		}
		// A typed error is not enough: rejecting what the user typed must be
		// reported as a user error, never as an internal fault.
		if problem.Category != errs.CategoryValidation ||
			problem.Subtype != errs.SubtypeInvalidArgument {
			unguarded = append(unguarded,
				fmt.Sprintf("%s (%s/%s)", c.CommandPath(), problem.Category, problem.Subtype))
		}
	})

	if checked == 0 {
		t.Fatal("no Args validator rejected the probe input; the walk found nothing to check")
	}
	if len(unguarded) > 0 {
		t.Errorf("these commands misreport a positional-argument mistake: %v", unguarded)
	}
}

// forEachCommand visits root and every command beneath it.
func forEachCommand(cmd *cobra.Command, visit func(*cobra.Command)) {
	visit(cmd)
	for _, sub := range cmd.Commands() {
		forEachCommand(sub, visit)
	}
}

// unrenderableTypedError is a problem carrier the envelope writer cannot
// serialize: the exported func field makes json.Marshal fail. A plugin's Wrap
// chain returning a value like this is the realistic way to reach the
// dispatcher's last-resort branch.
type unrenderableTypedError struct {
	*errs.Problem
	Leak func() `json:"leak"`
}

// TestUnrenderableTypedErrorStillReachesStderr pins why the last-resort branch
// rebuilds the error instead of reusing it: the value it receives has just
// failed to serialize, so handing the same value to the writer again would
// leave the user with a non-zero exit and a silent stderr.
func TestUnrenderableTypedErrorStillReachesStderr(t *testing.T) {
	t.Setenv("LARKSUITE_CLI_CONFIG_DIR", t.TempDir())

	broken := &unrenderableTypedError{
		Problem: &errs.Problem{
			Category: errs.CategoryInternal,
			Subtype:  errs.SubtypeUnknown,
			Message:  "upstream blew up",
		},
		Leak: func() {},
	}

	// Precondition: this value really cannot render itself.
	if output.WriteTypedErrorEnvelope(io.Discard, broken, "user") {
		t.Fatal("precondition failed: the probe error serialized successfully")
	}

	f, _, _, _ := cmdutil.TestFactory(t, nil)
	errOut := &bytes.Buffer{}
	f.IOStreams.ErrOut = errOut

	exit := handleRootError(f, broken, nil, stageCommandBody)

	if errOut.Len() == 0 {
		t.Fatal("stderr is empty; a non-zero exit must always be explained")
	}
	var envelope struct {
		Error struct {
			Category errs.Category `json:"type"`
			Message  string        `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(errOut.Bytes(), &envelope); err != nil {
		t.Fatalf("stderr is not a JSON envelope: %v\n%s", err, errOut.String())
	}
	if envelope.Error.Message != "upstream blew up" {
		t.Errorf("message = %q, want the original text preserved", envelope.Error.Message)
	}
	if want := output.ExitCodeOf(broken); exit != want {
		t.Errorf("exit = %d, want %d; the rebuilt error must keep the original category's exit code",
			exit, want)
	}
}

// TestShortcutPositionalErrorNamesTheStrayWord pins the diagnostics the
// shortcut framework adds over the generic Args wrapper: which word was
// unexpected, and where to look for the flags to use instead.
func TestShortcutPositionalErrorNamesTheStrayWord(t *testing.T) {
	tmpHome(t)
	quietNotices(t)

	_, _, stderr := executeWithCapturedOS(t, nil, "wiki", "+space-list", "stray")

	var envelope struct {
		Error struct {
			Hint  string `json:"hint"`
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
		t.Fatalf("stderr is not a JSON envelope: %v\n%s", err, stderr)
	}
	if !strings.Contains(envelope.Error.Hint, "--help") {
		t.Errorf("hint = %q, want it to point at --help", envelope.Error.Hint)
	}
	if envelope.Error.Param != "stray" {
		t.Errorf("param = %q, want the stray word named", envelope.Error.Param)
	}
}

// TestClassificationIgnoresErrorText is the guard that keeps error-text
// matching from creeping back into classification. The category must follow
// where dispatch failed, not what the message happens to say: the same text
// classified in two stages must land in different categories, and unrelated
// texts in one stage must land in the same one. Any reintroduced text
// whitelist breaks one half or the other.
func TestClassificationIgnoresErrorText(t *testing.T) {
	const sharedText = `required flag(s) "csv" not set`

	validationCause := errors.New(sharedText)
	bodyCause := errors.New(sharedText)
	fromValidation := normalizeRootError(validationCause, stageUserInput)
	fromBody := normalizeRootError(bodyCause, stageCommandBody)

	// Both stages must keep the original reachable; a rebuild that only
	// carries the message forward loses it for whoever inspects the chain.
	if !errors.Is(fromValidation, validationCause) {
		t.Error("stageUserInput dropped the original error from the chain")
	}
	if !errors.Is(fromBody, bodyCause) {
		t.Error("stageCommandBody dropped the original error from the chain")
	}

	vp, ok := errs.ProblemOf(fromValidation)
	if !ok {
		t.Fatalf("stageUserInput produced an unclassified error: %v", fromValidation)
	}
	bp, ok := errs.ProblemOf(fromBody)
	if !ok {
		t.Fatalf("stageCommandBody produced an unclassified error: %v", fromBody)
	}
	if vp.Category == bp.Category {
		t.Errorf("identical text got the same category %q in both stages; "+
			"classification must depend on the stage, not the text", vp.Category)
	}
	if vp.Category != errs.CategoryValidation {
		t.Errorf("stageUserInput category = %q, want %q", vp.Category, errs.CategoryValidation)
	}
	if bp.Category != errs.CategoryInternal {
		t.Errorf("stageCommandBody category = %q, want %q", bp.Category, errs.CategoryInternal)
	}

	// Texts cobra emits, texts it does not, and a message with no relation to
	// argument handling all describe the same thing in one stage: the command
	// line did not pass validation.
	for _, text := range []string{
		`unknown command "nope" for "lark-cli"`,
		"accepts 1 arg(s), received 2",
		"at least one of the flags in the group [start-cell range] is required",
		"positional arguments are not supported",
		"a message no version of cobra has ever produced",
		"",
	} {
		got := normalizeRootError(errors.New(text), stageUserInput)
		p, ok := errs.ProblemOf(got)
		if !ok {
			t.Fatalf("text %q produced an unclassified error", text)
		}
		if p.Category != errs.CategoryValidation || p.Subtype != errs.SubtypeInvalidArgument {
			t.Errorf("text %q classified as %s/%s, want %s/%s — the text must not matter",
				text, p.Category, p.Subtype, errs.CategoryValidation, errs.SubtypeInvalidArgument)
		}
	}
}

// TestInstrumentErrorStagesCoversBothBodyForms pins that the entry mark is set
// for either shape a command body can take. Cobra prefers RunE when both are
// present, so a command declaring only Run would otherwise leave the mark
// unset and have its errors attributed to the user.
func TestInstrumentErrorStagesCoversBothBodyForms(t *testing.T) {
	t.Run("nil root is a no-op", func(t *testing.T) {
		instrumentErrorStages(nil) // must not panic
	})

	t.Run("Run-only command marks entry", func(t *testing.T) {
		root := &cobra.Command{Use: "root"}
		ran := false
		leaf := &cobra.Command{Use: "legacy", Run: func(*cobra.Command, []string) { ran = true }}
		root.AddCommand(leaf)

		instrumentErrorStages(root)
		if currentErrorStage() != stageUserInput {
			t.Fatal("stage must start at user input before any body runs")
		}
		leaf.Run(leaf, nil)
		if !ran {
			t.Error("the original Run was not invoked through the wrapper")
		}
		if currentErrorStage() != stageCommandBody {
			t.Error("entering a Run body did not mark the command-body stage")
		}
	})

	t.Run("RunE-only command marks entry and propagates its error", func(t *testing.T) {
		root := &cobra.Command{Use: "root"}
		want := errors.New("body failed")
		leaf := &cobra.Command{Use: "modern", RunE: func(*cobra.Command, []string) error { return want }}
		root.AddCommand(leaf)

		instrumentErrorStages(root)
		if currentErrorStage() != stageUserInput {
			t.Fatal("stage must start at user input before any body runs")
		}
		if got := leaf.RunE(leaf, nil); !errors.Is(got, want) {
			t.Errorf("RunE error = %v, want %v passed through the wrapper", got, want)
		}
		if currentErrorStage() != stageCommandBody {
			t.Error("entering a RunE body did not mark the command-body stage")
		}
	})
}

// TestWrapperFailureIsOurFaultNotTheUsers pins that a plugin wrapper failing
// before it delegates is attributed to us. The wrapper chain is part of
// executing the command, so its failure is never a mistake in what the user
// typed — reporting it as invalid input would send the user looking at their
// own command line for a fault that is not there.
func TestWrapperFailureIsOurFaultNotTheUsers(t *testing.T) {
	tmpHome(t)
	quietNotices(t)
	platform.ResetForTesting()
	t.Cleanup(platform.ResetForTesting)

	platform.Register(platform.NewPlugin("backend", "1.0").
		Wrap("gate", platform.All(), func(next platform.Handler) platform.Handler {
			return func(context.Context, platform.Invocation) error {
				// Fails without delegating, and without using AbortError.
				return errors.New("plugin backend unavailable")
			}
		}).FailOpen().MustBuild())

	code, _, stderr := executeWithCapturedOS(t, nil, "profile", "list")

	var envelope struct {
		Error struct {
			Category errs.Category `json:"type"`
			Subtype  errs.Subtype  `json:"subtype"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
		t.Fatalf("stderr is not a JSON envelope: %v\n%s", err, stderr)
	}
	if envelope.Error.Category != errs.CategoryInternal ||
		envelope.Error.Subtype != errs.SubtypeUnknown {
		t.Errorf("reported as %s/%s, want %s/%s",
			envelope.Error.Category, envelope.Error.Subtype,
			errs.CategoryInternal, errs.SubtypeUnknown)
	}
	if code != output.ExitInternal {
		t.Errorf("exit code = %d, want %d", code, output.ExitInternal)
	}
}

// TestShutdownHookCannotRewriteExitCodeOnlySignals pins the same protection for
// the two signals that carry no Problem. They are plain structs with an
// exported Code, so nothing stops a handler from writing to them; what protects
// the user is that the exit code is already settled before the event fires.
func TestShutdownHookCannotRewriteExitCodeOnlySignals(t *testing.T) {
	cfgDir := tmpHome(t)
	quietNotices(t)
	writeAppConfigWithoutUser(t, cfgDir)
	platform.ResetForTesting()
	t.Cleanup(platform.ResetForTesting)

	rewrote := false
	platform.Register(platform.NewPlugin("tamper", "1.0").
		On(platform.Shutdown, "rewrite",
			func(_ context.Context, lc *platform.LifecycleContext) error {
				var bare *output.BareError
				if errors.As(lc.Err, &bare) {
					bare.Code = 0
					rewrote = true
				}
				var partial *output.PartialFailureError
				if errors.As(lc.Err, &partial) {
					partial.Code = 0
					rewrote = true
				}
				return nil
			}).
		MustBuild())

	code, _, _ := executeWithCapturedOS(t, nil,
		"auth", "check", "--scope", "im:message:send_as_bot")

	if !rewrote {
		t.Fatal("the handler never saw an exit-code-only signal; the test no longer covers its own premise")
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1; a Shutdown handler must not be able to change it", code)
	}
}

// TestShutdownHandlersDoNotSeeEachOthersEdits pins that handlers are isolated
// from one another. They run in registration order against the same failure, so
// sharing one error value would let whichever runs first decide what every
// later audit handler records.
func TestShutdownHandlersDoNotSeeEachOthersEdits(t *testing.T) {
	tmpHome(t)
	quietNotices(t)
	platform.ResetForTesting()
	t.Cleanup(platform.ResetForTesting)

	var secondSaw errs.Category
	secondRan := false
	platform.Register(platform.NewPlugin("tamper", "1.0").
		On(platform.Shutdown, "rewrite",
			func(_ context.Context, lc *platform.LifecycleContext) error {
				if p, ok := errs.ProblemOf(lc.Err); ok {
					p.Category = errs.CategoryNetwork
					p.Subtype = "rewritten_by_first_handler"
				}
				return nil
			}).
		MustBuild())
	platform.Register(platform.NewPlugin("audit", "1.0").
		On(platform.Shutdown, "observe",
			func(_ context.Context, lc *platform.LifecycleContext) error {
				secondRan = true
				if p, ok := errs.ProblemOf(lc.Err); ok {
					secondSaw = p.Category
				}
				return nil
			}).
		MustBuild())

	executeWithCapturedOS(t, nil, "definitely-not-a-command")

	if !secondRan {
		t.Fatal("the second handler never ran; the test no longer covers its own premise")
	}
	if secondSaw != errs.CategoryValidation {
		t.Errorf("second handler observed %q, want %q — the first handler's edit leaked",
			secondSaw, errs.CategoryValidation)
	}
}
