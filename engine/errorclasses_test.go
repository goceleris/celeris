package engine

import (
	"reflect"
	"strings"
	"testing"

	"github.com/goceleris/celeris/engine/internal/errclass"
)

// TestFillErrorClassesSetsEveryBucket is the celeris#645 wiring guard.
//
// FillErrorClasses is the single place an errclass bucket becomes an
// EngineMetrics field, and it is a hand-written field-by-field copy — the same
// shape as the adaptive aggregator that silently dropped ten fields in #627
// and seven more in #642. A bucket left out here is published as 0 on EVERY
// engine while its share still counts toward ErrorCount, so the breakdown
// stops adding up to the total and nothing says which bucket went missing.
//
// Reflective on purpose: an enumerated test omits the next field exactly the
// way the copy does.
func TestFillErrorClassesSetsEveryBucket(t *testing.T) {
	var src errclass.Snapshot
	sv := reflect.ValueOf(&src).Elem()
	for i := range sv.NumField() {
		sv.Field(i).SetUint(uint64(i + 1))
	}

	var m EngineMetrics
	FillErrorClasses(&m, src)

	mv := reflect.ValueOf(m)
	mt := mv.Type()
	var zero []string
	for i := range mv.NumField() {
		name := mt.Field(i).Name
		if name == "ErrorCount" || !strings.HasPrefix(name, "Error") {
			continue
		}
		if mv.Field(i).IsZero() {
			zero = append(zero, name)
		}
	}
	if len(zero) > 0 {
		t.Errorf("FillErrorClasses left %d Error* field(s) at zero: %v\n"+
			"Every errclass bucket must be copied into its EngineMetrics field "+
			"(celeris#645).", len(zero), zero)
	}
}

// TestEveryErrorBucketFieldHasABucket is the other direction: an Error* field
// declared on EngineMetrics but never fed by FillErrorClasses would read 0
// forever on every engine, which is how a metric looks when it is broken and
// how it looks when nothing went wrong.
func TestEveryErrorBucketFieldHasABucket(t *testing.T) {
	bucketFields := map[string]bool{}
	st := reflect.TypeOf(errclass.Snapshot{})
	for i := range st.NumField() {
		bucketFields["Error"+st.Field(i).Name] = true
	}

	mt := reflect.TypeOf(EngineMetrics{})
	for i := range mt.NumField() {
		name := mt.Field(i).Name
		// ErrorCount is the derived total; StandbyErrorCount is the
		// adaptive engine's own one-sided split, not a cause bucket.
		if !strings.HasPrefix(name, "Error") || name == "ErrorCount" {
			continue
		}
		if !bucketFields[name] {
			t.Errorf("EngineMetrics.%s looks like an ErrorCount bucket but has no "+
				"matching errclass.Snapshot field, so nothing can ever set it "+
				"(celeris#645).", name)
		}
	}
}

// TestFillErrorClassesDerivesErrorCountFromTheBuckets pins the invariant the
// whole split rests on: the published total IS the published parts. celeris
// #645 could only bound its answer because ErrorCount was a separate atomic
// that no breakdown had to agree with.
func TestFillErrorClassesDerivesErrorCountFromTheBuckets(t *testing.T) {
	var src errclass.Snapshot
	sv := reflect.ValueOf(&src).Elem()
	var want uint64
	for i := range sv.NumField() {
		n := uint64(i+1) * 7
		sv.Field(i).SetUint(n)
		want += n
	}

	// A pre-existing value must be overwritten, not added to: ErrorCount is
	// assigned from the buckets and nowhere else.
	m := EngineMetrics{ErrorCount: 999}
	FillErrorClasses(&m, src)
	if m.ErrorCount != want {
		t.Errorf("ErrorCount = %d, want %d (the sum of the buckets)", m.ErrorCount, want)
	}

	var sum uint64
	mv := reflect.ValueOf(m)
	mt := mv.Type()
	for i := range mv.NumField() {
		name := mt.Field(i).Name
		if !strings.HasPrefix(name, "Error") || name == "ErrorCount" {
			continue
		}
		sum += mv.Field(i).Uint()
	}
	if sum != m.ErrorCount {
		t.Errorf("the Error* buckets sum to %d but ErrorCount is %d — the breakdown "+
			"must account for the whole total", sum, m.ErrorCount)
	}
}
