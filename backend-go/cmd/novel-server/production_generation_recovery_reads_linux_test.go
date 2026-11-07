//go:build linux

package main

import (
	"testing"
)

// TestProductionGenerationRecoveryReadBuckets re-reads the bucket table of
// generation:list-recovery and pins the two refused names at the lane's bound
// set: editors and batches stay served by their sibling names, while
// generation:list-recovery and generation:find-blueprint-recovery stay unbound.
func TestProductionGenerationRecoveryReadBuckets(t *testing.T) {
	buckets := productionGenerationRecoveryBuckets()
	byName := make(map[string]bool, len(buckets))
	for _, bucket := range buckets {
		if byName[bucket.Name] {
			t.Fatalf("the bucket table lists %v twice", bucket.Name)
		}
		byName[bucket.Name] = true
		productionRecoveryMarkers(t, "generation:list-recovery bucket "+bucket.Name, bucket.Evidence)
	}
	for _, name := range []string{"editors", "batches"} {
		if !byName[name] {
			t.Fatalf("the bucket table does not name the served %v bucket", name)
		}
	}
	for _, name := range []string{"planning", "drafts", "reviews"} {
		if !byName[name] {
			t.Fatalf("the bucket table does not name the refused %v bucket", name)
		}
	}
	if err := bindProductionGenerationRecoveryReads(nil); err == nil || err.Error() != "TRANSPORT_SERVER_REQUIRED" {
		t.Fatalf("the bucket binder accepted a nil server: %v", err)
	}

	// The bucket table is verified against the finished lane, not against the
	// table's own claims: the served buckets must really be bound, the refused
	// names must really stay unbound.
	lane := productionRecoveryOpen(t, nil)
	bound := lane.server.BoundOperations()
	for _, name := range []string{"editor-inline:read-recovery", "generation:list-batches"} {
		if !productionRecoveryBoundContains(bound, name) {
			t.Fatalf("the served %v bucket is not bound in the finished lane", name)
		}
	}
	for _, name := range []string{productionGenerationRecoveryList, productionGenerationRecoveryFind} {
		if productionRecoveryBoundContains(bound, name) {
			t.Fatalf("%v is bound in the finished lane after all", name)
		}
	}
	if err := bindProductionGenerationRecoveryReads(lane.server); err != nil {
		t.Fatalf("the bucket binder refused the finished lane: %v", err)
	}
}
