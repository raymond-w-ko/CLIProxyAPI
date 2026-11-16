package auth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDurableAffinityPoolCooldownUsesEligibleOwner(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, tc := range []struct {
		name, rebind string
		quota        bool
		mayMigrate   bool
	}{
		{"generic 429", "quota", false, false},
		{"confirmed quota", "quota", true, true},
		{"never", "never", true, false},
	} {
		for _, siblingCooling := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/siblingCooling=%t", tc.name, siblingCooling), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "bindings.json")
				manager, ids := durableTestManagerWithRebindOn(t, path, tc.rebind)
				opts := durableTestOptions()
				if _, errPick := durableTestPick(manager, "model-one", opts); errPick != nil {
					t.Fatal(errPick)
				}
				hour, minute := time.Hour, time.Minute
				manager.MarkResult(context.Background(), Result{AuthID: ids[0], Provider: "claude", Model: "model-one", Options: opts,
					Error: &Error{HTTPStatus: 429, Message: tc.name}, CredentialScope: tc.quota, RetryAfter: &hour})
				if siblingCooling {
					manager.MarkResult(context.Background(), Result{AuthID: ids[1], Provider: "claude", Model: "model-one", Options: opts,
						Error: &Error{HTTPStatus: 429, Message: "rate limited"}, RetryAfter: &minute})
				}
				before, errRead := os.ReadFile(path)
				if errRead != nil {
					t.Fatal(errRead)
				}
				now := time.Now()
				deadline, available, ok := manager.earliestPoolCooldownForModelAt(context.Background(), []string{"claude"}, "model-one", opts, now)
				if tc.mayMigrate && !siblingCooling {
					if !available || ok {
						t.Fatalf("healthy replacement not eligible: deadline=%v available=%t ok=%t", deadline, available, ok)
					}
				} else {
					id := ids[0]
					if tc.mayMigrate {
						id = ids[1]
					}
					auth, _ := manager.GetByID(id)
					want := auth.ModelStates["model-one"].NextRetryAfter
					if available || !ok || !deadline.Equal(want) {
						t.Fatalf("deadline=%v available=%t ok=%t; want eligible recovery %v", deadline, available, ok, want)
					}
				}
				after, errRead := os.ReadFile(path)
				if errRead != nil || string(after) != string(before) {
					t.Fatalf("cooldown probe changed ownership: %v", errRead)
				}
			})
		}
	}
}
