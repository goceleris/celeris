package errclass

import (
	"reflect"
	"testing"
)

// fillSnapshot gives every bucket a distinct nonzero value, so a sum that
// double-counts one or skips one lands on a different number than the right
// answer. Powers of two make any such mistake unambiguous rather than
// coincidentally equal.
func fillSnapshot() (Snapshot, uint64) {
	var s Snapshot
	v := reflect.ValueOf(&s).Elem()
	var want uint64
	for i := range v.NumField() {
		f := v.Field(i)
		if f.Kind() != reflect.Uint64 {
			panic("errclass.Snapshot gained a non-uint64 field " +
				v.Type().Field(i).Name + "; Total and this test both assume uint64 buckets")
		}
		n := uint64(1) << i
		f.SetUint(n)
		want += n
	}
	return s, want
}

// TestTotalCoversEveryBucket is the celeris#645 guard.
//
// ErrorCount is now DERIVED from the buckets rather than counted beside them,
// which is the whole point: a total and a breakdown that are maintained
// separately drift, and the drift is invisible. But Total() is still a hand-
// written sum, so a bucket added to Snapshot and forgotten there would
// silently shrink every engine's ErrorCount — the counter would start
// UNDER-reporting, which is worse than the single number it replaced.
//
// The check is reflective on purpose, for the same reason
// adaptive's TestMetricsCarriesEveryFieldReflectively is: the failure mode is
// an omission, and a test that enumerates today's buckets omits tomorrow's
// exactly the way the sum did.
func TestTotalCoversEveryBucket(t *testing.T) {
	s, want := fillSnapshot()
	if got := s.Total(); got != want {
		v := reflect.ValueOf(s)
		var missing []string
		for i := range v.NumField() {
			// Each bucket holds a distinct power of two, so the sum
			// carries nowhere and a bucket left out of it is exactly the
			// bit missing from got.
			if got&(1<<i) == 0 {
				missing = append(missing, v.Type().Field(i).Name)
			}
		}
		t.Errorf("Snapshot.Total() = %d, want %d; bucket(s) missing from the sum: %v\n"+
			"Every field of errclass.Snapshot is a share of EngineMetrics.ErrorCount "+
			"and must be added into Total (celeris#645).", got, want, missing)
	}
}

// TestCountersTotalMatchesTheLiveAtomics pins the other half: Counters.Total
// goes through Snapshot, so a bucket present in Snapshot but never read in
// Counters.Snapshot would also under-report. Bump every atomic once and the
// total must be the number of buckets.
func TestCountersTotalMatchesTheLiveAtomics(t *testing.T) {
	var c Counters
	v := reflect.ValueOf(&c).Elem()
	n := v.NumField()
	for i := range n {
		f := v.Field(i).Addr().Interface()
		adder, ok := f.(interface{ Add(uint64) uint64 })
		if !ok {
			t.Fatalf("errclass.Counters.%s is not an atomic.Uint64", v.Type().Field(i).Name)
		}
		adder.Add(1)
	}
	if got := c.Total(); got != uint64(n) {
		t.Errorf("Counters.Total() = %d after bumping all %d buckets once, want %d; "+
			"a bucket is missing from Counters.Snapshot or from Snapshot.Total", got, n, n)
	}
}

// TestSnapshotAndCountersHaveTheSameBuckets keeps the value struct and the
// atomic struct from drifting apart: a bucket added to one and not the other
// is either unreadable (Counters only) or never written (Snapshot only), and
// both failures are silent zeros in the published metrics.
func TestSnapshotAndCountersHaveTheSameBuckets(t *testing.T) {
	ct := reflect.TypeOf(Counters{})
	st := reflect.TypeOf(Snapshot{})
	if ct.NumField() != st.NumField() {
		t.Fatalf("Counters has %d buckets, Snapshot has %d", ct.NumField(), st.NumField())
	}
	for i := range ct.NumField() {
		if ct.Field(i).Name != st.Field(i).Name {
			t.Errorf("bucket %d: Counters.%s vs Snapshot.%s — the two structs must "+
				"declare the same buckets in the same order",
				i, ct.Field(i).Name, st.Field(i).Name)
		}
	}
}
