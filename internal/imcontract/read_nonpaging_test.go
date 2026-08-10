// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package imcontract

import (
	"strings"
	"testing"

	"github.com/larksuite/cli/errs"
	"github.com/larksuite/cli/internal/output"
)

// nonPagingReadContracts returns every registered read contract that carries no
// pagination. Driving this from the registry keeps new contracts covered
// without anyone maintaining a list.
func nonPagingReadContracts(t *testing.T) []Contract {
	t.Helper()
	var out []Contract
	for _, contract := range All() {
		if !contract.Strategy.Kind.IsRead() {
			continue
		}
		session, err := NewReadSession(contract, ReadOptions{})
		if err != nil {
			t.Fatalf("NewReadSession(%s) error = %v", contract.Key, err)
		}
		if !session.RequiresPagination() {
			out = append(out, contract)
		}
	}
	return out
}

// TestNonPagingReadContractsTolerateAbsentPagination locks the core invariant:
// a read contract that declares no pagination must not be rejected for failing
// to supply pagination metadata. Entity and materialize reads answer by ID or
// download a single resource, so they have no pagination fact to report.
func TestNonPagingReadContractsTolerateAbsentPagination(t *testing.T) {
	contracts := nonPagingReadContracts(t)
	if len(contracts) == 0 {
		t.Fatal("no non-paginating read contracts found; the assertion would be vacuous")
	}
	for _, contract := range contracts {
		t.Run(string(contract.Key), func(t *testing.T) {
			// The guard trips on meta == nil || meta.Pages < 1, so cover both.
			for name, meta := range map[string]*output.PaginationMeta{
				"nil_meta":        nil,
				"zero_pages_meta": {},
			} {
				t.Run(name, func(t *testing.T) {
					session, err := NewReadSession(contract, ReadOptions{})
					if err != nil {
						t.Fatalf("NewReadSession() error = %v", err)
					}
					if err := session.ObserveOutputPagination(meta, false); err != nil {
						t.Fatalf("ObserveOutputPagination() error = %v, want nil", err)
					}
					result, err := session.Finalize(map[string]any{"probe": "value"})
					if err != nil {
						t.Fatalf("Finalize() error = %v, want nil", err)
					}
					if !result.OK {
						t.Fatal("Finalize() OK = false, want true")
					}
				})
			}
		})
	}
}

// TestPagingReadContractsStillRejectAbsentPagination is the reverse assertion:
// relaxing the guard for non-paginating reads must not degrade into "no read
// ever checks pagination". Paginated contracts keep failing closed.
func TestPagingReadContractsStillRejectAbsentPagination(t *testing.T) {
	checked := 0
	for _, contract := range All() {
		if !contract.Strategy.Kind.IsRead() {
			continue
		}
		session, err := NewReadSession(contract, ReadOptions{})
		if err != nil {
			t.Fatalf("NewReadSession(%s) error = %v", contract.Key, err)
		}
		if !session.RequiresPagination() {
			continue
		}
		checked++
		if err := session.ObserveOutputPagination(nil, false); err == nil {
			t.Errorf("%s: ObserveOutputPagination(nil) = nil, want error (fail closed)", contract.Key)
		}
	}
	if checked == 0 {
		t.Fatal("no paginating read contracts found; the assertion would be vacuous")
	}
}

// TestPagingReadGuardMessageIsDiagnosable pins the fail-closed error to a local
// contract diagnosis: it must not blame the server response, must offer a single
// action, and must name the offending contract.
func TestPagingReadGuardMessageIsDiagnosable(t *testing.T) {
	var contract Contract
	for _, c := range All() {
		if !c.Strategy.Kind.IsRead() {
			continue
		}
		session, err := NewReadSession(c, ReadOptions{})
		if err != nil {
			t.Fatalf("NewReadSession(%s) error = %v", c.Key, err)
		}
		if session.RequiresPagination() {
			contract = c
			break
		}
	}
	if contract.Key == "" {
		t.Fatal("no paginating read contract found")
	}
	session, err := NewReadSession(contract, ReadOptions{})
	if err != nil {
		t.Fatalf("NewReadSession() error = %v", err)
	}
	err = session.ObserveOutputPagination(nil, false)
	if err == nil {
		t.Fatal("ObserveOutputPagination(nil) = nil, want error")
	}
	problem, ok := errs.ProblemOf(err)
	if !ok {
		t.Fatalf("error is not typed: %v", err)
	}
	if problem.Hint == "" {
		t.Fatal("fail-closed error has no hint; the reader gets no recoverable action")
	}
	text := strings.ToLower(problem.Message + " " + problem.Hint)
	for _, blamed := range []string{"invalid pagination metadata", "invalid response"} {
		if strings.Contains(text, blamed) {
			t.Errorf("message blames the response (%q); it is a local contract invariant: %q", blamed, text)
		}
	}
	// The hint must offer exactly one action. Suggesting a different command or a
	// looser path is what pushed agents off the fail-closed semantics before.
	for _, misdirect := range []string{"retry", "instead use", "fall back", "--page"} {
		if strings.Contains(strings.ToLower(problem.Hint), misdirect) {
			t.Errorf("hint misdirects the reader with %q: %q", misdirect, problem.Hint)
		}
	}
	if !strings.Contains(problem.Message, string(contract.Key)) {
		t.Errorf("message does not name the offending contract %q: %q", contract.Key, problem.Message)
	}
}
