// Copyright (c) 2026 Lark Technologies Pte. Ltd.
// SPDX-License-Identifier: MIT

package imcontract

import (
	"testing"

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
