// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package cmd

import (
	"sync/atomic"

	"github.com/spf13/cobra"

	"github.com/larksuite/cli/errs"
)

// commandBodyEntered records whether dispatch reached a command's own body
// (Run / RunE) during this process. It is the signal errorStageOf uses to
// tell a user's input mistake apart from a fault in our own code, replacing
// the error-text matching this package used to do.
//
// cobra runs its own validation strictly before the command body:
// ValidateArgs, then PersistentPreRunE, then ValidateRequiredFlags and
// ValidateFlagGroups, then Run / RunE. So an error surfacing while this flag
// is still false came out of cobra's validation of what the user typed;
// every command body and every PersistentPreRunE in this repository returns
// a typed errs.* error, so an untyped error surfacing after it is set is a
// gap on our side.
var commandBodyEntered atomic.Bool

// errorStage distinguishes the two origins an untyped error can have.
type errorStage int

const (
	// stageUserInput means dispatch failed inside cobra's own validation of
	// the command line, before reaching any command body.
	stageUserInput errorStage = iota

	// stageCommandBody means a command body ran and produced an error that
	// never went through errs/, which is a missing conversion on our side.
	stageCommandBody
)

// currentErrorStage reads the mark once, right after dispatch returns. The
// classifiers take the result as an argument rather than reading the flag
// themselves, so they stay pure and each stage is directly testable.
func currentErrorStage() errorStage {
	if commandBodyEntered.Load() {
		return stageCommandBody
	}
	return stageUserInput
}

// instrumentErrorStages walks the built command tree and wraps two seams on
// every command:
//
//   - Args: cobra's own positional validators (ExactArgs, MaximumNArgs, ...)
//     return plain errors. Wrapping at the single place they are invoked
//     converts every one of them — including validators added later — into a
//     typed validation error, so no call site has to remember to do it.
//   - Run / RunE: entering a command body sets commandBodyEntered, which is
//     what tells a later untyped error apart from a user input mistake.
//
// Commands cobra registers lazily during Execute (completion, __complete) are
// deliberately not materialized here: leaving registration untouched keeps
// help output byte-identical, and the stage signal already classifies their
// argument errors correctly without the Args wrapper, since dispatch fails
// before any body runs.
func instrumentErrorStages(root *cobra.Command) {
	if root == nil {
		return
	}
	// Each built tree starts a fresh dispatch, so clear the mark here rather
	// than relying on process startup — a test that builds several trees in
	// one process would otherwise inherit the previous tree's entry.
	commandBodyEntered.Store(false)
	instrumentCommandStages(root)
}

func instrumentCommandStages(cmd *cobra.Command) {
	if inner := cmd.Args; inner != nil {
		cmd.Args = func(c *cobra.Command, args []string) error {
			return typedArgsError(inner(c, args))
		}
	}

	if inner := cmd.RunE; inner != nil {
		cmd.RunE = func(c *cobra.Command, args []string) error {
			commandBodyEntered.Store(true)
			return inner(c, args)
		}
	}
	if inner := cmd.Run; inner != nil {
		cmd.Run = func(c *cobra.Command, args []string) {
			commandBodyEntered.Store(true)
			inner(c, args)
		}
	}

	for _, sub := range cmd.Commands() {
		instrumentCommandStages(sub)
	}
}

// typedArgsError converts a positional-argument rejection into a typed
// validation error. A validator that already returns a typed error (the
// shortcut framework's own) is left alone, so it keeps the richer param and
// hint it produced.
func typedArgsError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := errs.ProblemOf(err); ok {
		return err
	}
	return errs.NewValidationError(errs.SubtypeInvalidArgument, "%s", err.Error()).
		WithCause(err)
}
