package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

// gateSource is the source the seal-gate tests register (#913-D9).
const gateSource = "anthropic-admin"

func gateDay(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }

func registerGated(t *testing.T, db *store.DB, sources ...string) {
	t.Helper()
	if _, err := db.RegisterSources(context.Background(), sources); err != nil {
		t.Fatal(err)
	}
}

func settle(t *testing.T, db *store.DB, from, through time.Time) {
	t.Helper()
	if err := db.AdvanceSourceWatermark(context.Background(), gateSource, from, through); err != nil {
		t.Fatal(err)
	}
}

// TestSealGate_UnfetchableGapIsNeverCertified (#913-D9 ruling C′ conditions 2
// and 7, the go seat's scenario): the Admin key is revoked on 25 June, after
// May sealed, and fixed on 3 August, whose pass re-reads only from 1 July. June
// is never sealed: the pass refuses it with source_behind, and the manifest and
// June's 404 name the gap. `--skip` records June as a gap only after its retry
// names that wait, and July, wholly inside the new run, then seals.
func TestSealGate_UnfetchableGapIsNeverCertified(t *testing.T) {
	h, db, s := newSealedReadHandler(t)
	ctx := context.Background()
	registerGated(t, db, gateSource)
	settle(t, db, gateDay(time.April, 1), gateDay(time.June, 24))
	sealPass(t, s)
	if n := sealedRows(t, db); n != 1 {
		t.Fatalf("control: %d months sealed at %s, want May alone", n, sealNow)
	}
	settle(t, db, gateDay(time.July, 1), gateDay(time.August, 2))
	s.now = func() time.Time { return gateDay(time.August, 20) }

	const gap = "anthropic-admin has an unfetchable gap from 2026-06-24T00:00:00Z to 2026-07-01T00:00:00Z"
	if err := s.sealDue(ctx); !errors.Is(err, errSourceBehind) || !strings.Contains(err.Error(), gap) {
		t.Fatalf("June's pass: %v, want errSourceBehind naming %q", err, gap)
	}
	if n := sealedRows(t, db); n != 1 {
		t.Fatalf("%d months sealed after June's refused pass, want May alone", n)
	}
	want := [3]string{"2026-06", store.SealGapSourceBehind, "2026-07-15T00:00:00Z"}
	if got := stallOf(t, sealedGet(t, h, "/api/v1/report_manifest")); got != want {
		t.Errorf("manifest: %v, want %v", got, want)
	}
	if rec := sealedGet(t, h, "/api/v1/scores?period=2026-06"); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), gap) {
		t.Errorf("June's read: %d %s; want a 404 naming the gap", rec.Code, rec.Body.String())
	}

	sk := newTestSealer(h)
	sk.now = s.now
	plan, err := sk.skip(ctx, "2026-06", "Admin key revoked 25 June (OPS-7)", false, "")
	if err != nil || plan.Sealed || plan.Category != store.SealGapSourceBehind || !strings.Contains(plan.Observed.Error(), gap) {
		t.Fatalf("skip plan: %+v, %v; want the source_behind wait named, nothing sealed", plan, err)
	}
	if _, err := sk.skip(ctx, "2026-06", "Admin key revoked 25 June (OPS-7)", true, store.SealGapSourceBehind); err != nil {
		t.Fatalf("confirmed skip: %v", err)
	}
	if n := gapRows(t, db); n != 1 {
		t.Fatalf("%d gaps after the confirmed skip, want June", n)
	}
	sealPass(t, s)
	if got := sealedOrder(t, db); !strings.HasSuffix(got, "2026-07-01T00:00:00Z") {
		t.Errorf("sealed after the skip: %s, want July sealed after the gap", got)
	}
}

// TestSealGate_RestartWaitsForThePollersFirstPass (#913-D9, the D9 brief's
// scenario): serve stopped at 00:30 on 1 June, so the poller settled only
// through 00:30 on 31 May; on restart the sealer's startup pass runs before the
// poller's and refuses May, logging at INFO which source it waits on. The
// poller's first pass re-reads from 1 May, contiguously, and the next seal
// pass seals May.
func TestSealGate_RestartWaitsForThePollersFirstPass(t *testing.T) {
	h, db, s := newSealedReadHandler(t)
	var logs bytes.Buffer
	h.logger = slog.New(slog.NewTextHandler(&logs, nil))
	registerGated(t, db, gateSource)
	settle(t, db, gateDay(time.April, 1), time.Date(2026, time.May, 31, 0, 30, 0, 0, time.UTC))
	s.now = func() time.Time { return gateDay(time.June, 16) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.run(ctx) }()
	eventually(t, "the startup pass", func() bool { return s.lastPass.Load() != 0 })
	cancel()
	<-done
	if n := sealedRows(t, db); n != 0 {
		t.Fatalf("%d months sealed before the poller's first pass, want none", n)
	}
	const waiting = "anthropic-admin has settled only through 2026-05-31T00:30:00Z"
	if st := s.stall.Load(); st == nil || st.code != store.SealGapSourceBehind || !strings.Contains(st.reason, waiting) {
		t.Errorf("stall after the startup pass: %+v, want source_behind naming %q", st, waiting)
	}
	if line := logs.String(); !strings.Contains(line, "level=INFO") || !strings.Contains(line, waiting) || strings.Contains(line, "level=ERROR") {
		t.Errorf("startup pass logged %q, want one INFO line naming %q", line, waiting)
	}

	settle(t, db, gateDay(time.May, 1), gateDay(time.June, 15))
	sealPass(t, s)
	if n := sealedRows(t, db); n != 1 {
		t.Errorf("%d months sealed after the poller's first pass, want May", n)
	}
}

// TestSealGate_ConfiguredSourcesOnly (#913-D9 conditions 3 and 5): after serve
// registered its sources, no row gates nothing; a registered source that has
// never passed gates every month, so a row without a watermark is never read
// as unconfigured; retiring it releases the gate. Before serve has registered
// its sources at all, every month waits, for the pass and for --arm alike.
func TestSealGate_ConfiguredSourcesOnly(t *testing.T) {
	_, db, s := newSealedReadHandler(t)
	registerGated(t, db, gateSource)
	err := s.sealDue(context.Background())
	if !errors.Is(err, errSourceBehind) || !strings.Contains(err.Error(), gateSource+" has not completed a successful pass") {
		t.Fatalf("registered, never passed: %v, want errSourceBehind naming %s", err, gateSource)
	}
	if n := sealedRows(t, db); n != 0 {
		t.Fatalf("%d months sealed while %s never passed, want none", n, gateSource)
	}
	registerGated(t, db)
	sealPass(t, s)
	if n := sealedRows(t, db); n != 1 {
		t.Errorf("%d months sealed once %s is retired, want May", n, gateSource)
	}

	_, db2, s2 := newSealedReadHandler(t)
	if _, err := rawSealStore(t, db2).Exec(`DELETE FROM source_registration`); err != nil {
		t.Fatal(err)
	}
	const unregistered = "tierd serve has not registered its sources on this database yet"
	if err := s2.sealDue(context.Background()); !errors.Is(err, errSourceBehind) || !strings.Contains(err.Error(), unregistered) {
		t.Errorf("no registration: pass %v, want errSourceBehind naming %q", err, unregistered)
	}
	if got, err := s2.arm(context.Background(), "2026-05"); !errors.Is(err, ErrSealArmRefused) || !strings.Contains(err.Error(), unregistered) {
		t.Errorf("no registration: arm %+v %v, want ErrSealArmRefused naming %q", got, err, unregistered)
	}
	if n := sealedRows(t, db2); n != 0 {
		t.Errorf("%d months sealed before serve registered its sources, want none", n)
	}
}

// TestSealGate_MonthOfAnEarlierRunStaysCertified (#913-D9 ruling R-8, the go
// seat's scenario): May, wholly inside the run from 1 April to 24 June, is
// still unsealed when a pass after a gap starts a new run on 1 July. The gap
// holds June alone: May seals, and the pass stops at June naming the gap.
func TestSealGate_MonthOfAnEarlierRunStaysCertified(t *testing.T) {
	_, db, s := newSealedReadHandler(t)
	registerGated(t, db, gateSource)
	settle(t, db, gateDay(time.April, 1), gateDay(time.June, 24))
	settle(t, db, gateDay(time.July, 1), gateDay(time.August, 2))
	s.now = func() time.Time { return gateDay(time.August, 20) }
	const gap = "anthropic-admin has an unfetchable gap from 2026-06-24T00:00:00Z to 2026-07-01T00:00:00Z"
	if err := s.sealDue(context.Background()); !errors.Is(err, errSourceBehind) || !strings.Contains(err.Error(), gap) {
		t.Errorf("pass: %v, want it stopped at June naming %q", err, gap)
	}
	if got := sealedOrder(t, db); got != "2026-05-01T00:00:00Z" {
		t.Errorf("sealed %q, want May alone", got)
	}
}

// TestSealGate_LostSpendHoldsItsMonthUntilSkipped (#913-D9 ruling R-8): a
// source whose run covers April to August but that lost spend on 10 June
// certifies May and July, never June: the pass stops at June naming the loss
// and the remedy, and once `--skip` records June, July seals.
func TestSealGate_LostSpendHoldsItsMonthUntilSkipped(t *testing.T) {
	_, db, s := newSealedReadHandler(t)
	ctx := context.Background()
	registerGated(t, db, gateSource)
	settle(t, db, gateDay(time.April, 1), gateDay(time.August, 2))
	if err := db.RecordSourceLoss(ctx, gateSource, gateDay(time.June, 10), gateDay(time.June, 10)); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return gateDay(time.August, 20) }
	const lost = "anthropic-admin lost spend from 2026-06-10T00:00:00Z to 2026-06-10T00:00:00Z that it can never re-read"
	if err := s.sealDue(ctx); !errors.Is(err, errSourceBehind) || !strings.Contains(err.Error(), lost) || !strings.Contains(err.Error(), "tierd seal --skip") {
		t.Fatalf("pass: %v, want it stopped at June naming %q and the remedy", err, lost)
	}
	if got := sealedOrder(t, db); got != "2026-05-01T00:00:00Z" {
		t.Fatalf("sealed %q, want May alone", got)
	}
	sk := newTestSealer(s.h)
	sk.now = s.now
	if _, err := sk.skip(ctx, "2026-06", "Opencode rows undecodable (OPS-9)", true, store.SealGapSourceBehind); err != nil {
		t.Fatalf("confirmed skip: %v", err)
	}
	sealPass(t, s)
	if got := sealedOrder(t, db); !strings.HasSuffix(got, "2026-07-01T00:00:00Z") {
		t.Errorf("sealed after the skip: %s, want July sealed after the gap", got)
	}
}

// TestSealGate_MonthBeforeTheFirstRunIsRefused (#913-D9): a source whose first
// run starts on 1 June, with no gap, has no record of May, so May's pass and
// `--arm` are both refused, naming the remedy.
func TestSealGate_MonthBeforeTheFirstRunIsRefused(t *testing.T) {
	_, db, s := newSealedReadHandler(t)
	registerGated(t, db, gateSource)
	settle(t, db, gateDay(time.June, 1), gateDay(time.July, 20))
	const first = "anthropic-admin has no record before 2026-06-01T00:00:00Z, its first recorded coverage"
	if err := s.sealDue(context.Background()); !errors.Is(err, errSourceBehind) || !strings.Contains(err.Error(), first) {
		t.Errorf("May's pass: %v, want errSourceBehind naming %q", err, first)
	}
	if got, err := s.arm(context.Background(), ""); !errors.Is(err, ErrSealArmRefused) || !strings.Contains(err.Error(), "arm a later month") {
		t.Errorf("arm earliest: %+v %v, want ErrSealArmRefused naming the remedy", got, err)
	}
	if n := sealedRows(t, db); n != 0 {
		t.Errorf("%d months sealed, want none", n)
	}
}

// TestSealGate_RecheckedInsideTheWriteTransaction (#913-D9): a source serve
// registers after the seal's read snapshot but before its write refuses the
// seal inside SealReport's transaction.
func TestSealGate_RecheckedInsideTheWriteTransaction(t *testing.T) {
	_, db, s := newSealedReadHandler(t)
	registerGated(t, db, gateSource)
	settle(t, db, gateDay(time.April, 1), gateDay(time.June, 24))
	s.beforeSeal = func() { registerGated(t, db, gateSource, "late-source") }
	if err := s.sealDue(context.Background()); !errors.Is(err, errSourceBehind) || !strings.Contains(err.Error(), "late-source has not completed a successful pass") {
		t.Errorf("pass: %v, want errSourceBehind naming late-source", err)
	}
	if n := sealedRows(t, db); n != 0 {
		t.Errorf("%d months sealed, want none", n)
	}
}

// TestSealGate_ArmRefused (#913-D9 condition 1): `tierd seal --arm` is refused,
// before and inside its seal, while a registered source has not settled past
// the month, and nothing is sealed or pinned.
func TestSealGate_ArmRefused(t *testing.T) {
	_, db, s := newSealFixture(t)
	registerGated(t, db, gateSource)
	for _, confirm := range []string{"", "2026-05"} {
		got, err := s.arm(context.Background(), confirm)
		if !errors.Is(err, ErrSealArmRefused) || !strings.Contains(err.Error(), gateSource) || got != (SealArm{}) {
			t.Errorf("arm %q: %+v %v; want ErrSealArmRefused naming %s", confirm, got, err, gateSource)
		}
	}

	_, db2, s2 := newSealFixture(t)
	s2.inSnapshot = func() { registerGated(t, db2, gateSource) }
	got, err := s2.arm(context.Background(), "2026-05")
	if !errors.Is(err, ErrSealArmRefused) || !strings.Contains(err.Error(), gateSource) || got != (SealArm{}) {
		t.Errorf("arm with the source registered inside its seal: %+v %v; want ErrSealArmRefused", got, err)
	}
	for i, d := range []*store.DB{db, db2} {
		if sealed, floor := armState(t, d); sealed != "" || floor != "" {
			t.Errorf("fixture %d: refused arm left sealed %q, floor %q; want nothing", i, sealed, floor)
		}
	}
}

// TestSealGate_ReadsKeepTheirMeaning (#913-D9 condition 1): a closed gate is not
// in lastSealable, so before a pass reports it every read is byte-identical to
// the same database with the source retired; after the pass refuses May, May
// reads as awaiting seal with the source named, never as not sealable, and
// June keeps its sealable_at.
func TestSealGate_ReadsKeepTheirMeaning(t *testing.T) {
	h, db, s := newSealedReadHandler(t)
	registerGated(t, db, gateSource)
	settle(t, db, gateDay(time.April, 1), gateDay(time.May, 20))
	s.now = func() time.Time { return time.Date(2026, time.June, 15, 0, 30, 0, 0, time.UTC) }
	paths := []string{"/api/v1/report_manifest", "/api/v1/scores?period=2026-05", "/api/v1/report_manifest?period=2026-06"}
	gated := map[string]string{}
	for _, p := range paths {
		gated[p] = sealedGet(t, h, p).Body.String()
	}
	registerGated(t, db)
	for _, p := range paths {
		if open := sealedGet(t, h, p).Body.String(); open != gated[p] {
			t.Errorf("%s with the gate closed:\n%s\nwith the source retired:\n%s", p, gated[p], open)
		}
	}
	if body := gated["/api/v1/report_manifest?period=2026-06"]; !strings.Contains(body, `"sealable_at":"2026-07-15T00:00:00Z"`) {
		t.Errorf("June's 404 with the gate closed: %s, want its sealable_at", body)
	}

	registerGated(t, db, gateSource)
	if err := s.sealDue(context.Background()); !errors.Is(err, errSourceBehind) {
		t.Fatalf("control: May's pass: %v, want errSourceBehind", err)
	}
	may := sealedGet(t, h, "/api/v1/scores?period=2026-05").Body.String()
	if !strings.Contains(may, errAwaitingSeal.Error()) || !strings.Contains(may, gateSource) || strings.Contains(may, errPeriodNotSealable.Error()) {
		t.Errorf("May's read after the refused pass: %s; want awaiting seal naming %s", may, gateSource)
	}
	if june := sealedGet(t, h, "/api/v1/report_manifest?period=2026-06").Body.String(); !strings.Contains(june, `"sealable_at":"2026-07-15T00:00:00Z"`) {
		t.Errorf("June's 404 after the refused pass: %s, want its sealable_at", june)
	}
}

// TestSealGate_BehindSourceNotUnreported (#913-D9 condition 4): a restarted
// server with no pass's failure in memory names an overdue month a source is
// behind with source_behind, never unreported, and SealStatus, --status's
// database read, names the source too.
func TestSealGate_BehindSourceNotUnreported(t *testing.T) {
	h, db, _ := newSealedReadHandler(t)
	registerGated(t, db, gateSource)
	settle(t, db, gateDay(time.April, 1), gateDay(time.May, 20))
	want := [3]string{"2026-05", store.SealGapSourceBehind, "2026-06-15T00:00:00Z"}
	if got := stallOf(t, sealedGet(t, h, "/api/v1/report_manifest")); got != want {
		t.Errorf("overdue May with no pass reporting: %v, want %v", got, want)
	}
	st, err := h.SealStatus(context.Background(), sealGrace, "2020-01")
	if err != nil || st.Owed != "2026-05" || !strings.Contains(st.Behind, gateSource+" has settled only through 2026-05-20T00:00:00Z") {
		t.Errorf("SealStatus: %+v %v; want May owed and %s named behind", st, err, gateSource)
	}
}
