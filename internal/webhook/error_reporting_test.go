package webhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tiermetric/tier/internal/store"
)

type pushErrorStore struct {
	Store
	lookupErr, captureErr error
	captureErrs           map[string]error
}

func (s *pushErrorStore) OutcomeByMergeCommit(context.Context, string) (store.Outcome, bool, error) {
	return store.Outcome{}, false, s.lookupErr
}

func (s *pushErrorStore) RecordPushCommit(_ context.Context, _ store.Outcome, _, commitSHA string, _ time.Time) (store.PushCommitStatus, error) {
	if err, ok := s.captureErrs[commitSHA]; ok {
		return 0, err
	}
	return 0, s.captureErr
}

func TestHandlePush_JoinsRevertAndCaptureErrors(t *testing.T) {
	lookupErr := errors.New("injected revert lookup failure")
	captureErr := errors.New("injected capture failure")
	s := &pushErrorStore{Store: newFakeStore(), lookupErr: lookupErr, captureErr: captureErr}
	h := New(s, testSecret, quietLogger(), WithPushCapture(nil))
	body := []byte(fmt.Sprintf(`{
		"ref":"refs/heads/main",
		"repository":{"default_branch":"main","full_name":"acme/app"},
		"commits":[
			{"id":"revert-one\nforged", "message":"Revert \"fix\"\n\nThis reverts commit %040x."},
			{"id":"revert-two", "message":"Revert \"fix\"\n\nThis reverts commit %040x."},
			{"id":"capture-sha", "message":"fix: closes #42", "author":{"username":"alice"}}
		]
	}`, 1, 2))
	err := h.handlePush(t.Context(), body)
	for _, cause := range []error{lookupErr, captureErr} {
		if !errors.Is(err, cause) {
			t.Errorf("handlePush error=%v, want cause %v", err, cause)
		}
	}
	if err == nil {
		t.Fatal("handlePush succeeded despite store failures")
	}
	for _, context := range []string{
		"lookup outcome by merge commit", "capture push outcomes",
		logSafeStr("revert-one\nforged"), logSafeStr("revert-two"), logSafeStr("capture-sha"),
	} {
		if !strings.Contains(err.Error(), context) {
			t.Errorf("handlePush error=%v, missing context %q", err, context)
		}
	}
	// Joining diagnostics must preserve the delivery's failure status.
	r := boundedRequest("push", strings.NewReader(string(body)))
	r.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status=%d, want 500", w.Code)
	}
}

func TestWebhookPushJoinedErrorsStatus(t *testing.T) {
	ioErr := errors.New("store I/O failure")
	lockErr := fmt.Errorf("record commit: %w", store.ErrWriteLockUnavailable)
	busyErr := errors.New("SQLITE_BUSY: database is locked")
	storeLockErr := fmt.Errorf("%w: %w", store.ErrWriteLockUnavailable, busyErr)
	for _, tc := range []struct {
		name                  string
		lookupErr, captureErr error
		wantStatus            int
		wantLevel             string
		captureErrs           map[string]error
	}{
		{"mixed", ioErr, lockErr, http.StatusInternalServerError, "ERROR", nil},
		{"mixed_reverse", lockErr, ioErr, http.StatusInternalServerError, "ERROR", nil},
		{"lock_only", lockErr, lockErr, http.StatusServiceUnavailable, "WARN", nil},
		{"real_shape_lock_only", nil, storeLockErr, http.StatusServiceUnavailable, "WARN", nil},
		{"nested_lock_only", errors.Join(lockErr, errors.Join(lockErr)), lockErr, http.StatusServiceUnavailable, "WARN", nil},
		{"mixed_capture_commits", nil, nil, http.StatusInternalServerError, "ERROR", map[string]error{
			"capture-sha": lockErr,
			"capture-two": io.ErrUnexpectedEOF,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			s := &pushErrorStore{Store: newFakeStore(), lookupErr: tc.lookupErr, captureErr: tc.captureErr, captureErrs: tc.captureErrs}
			h := New(s, testSecret, slog.New(slog.NewJSONHandler(&logs, nil)), WithPushCapture(nil))
			body := []byte(fmt.Sprintf(`{
				"ref":"refs/heads/main",
				"repository":{"default_branch":"main","full_name":"acme/app"},
				"commits":[
					{"id":"revert-sha", "message":"Revert \"fix\"\n\nThis reverts commit %040x."},
					{"id":"capture-sha", "message":"fix: closes #42", "author":{"username":"alice"}},
					{"id":"capture-two", "message":"fix: closes #43", "author":{"username":"alice"}}
				]
			}`, 1))
			r := boundedRequest("push", bytes.NewReader(body))
			r.Header.Set("X-Hub-Signature-256", sign(testSecret, body))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.wantStatus {
				t.Errorf("status=%d, want %d", w.Code, tc.wantStatus)
			}
			wantRetry := ""
			if tc.wantStatus == http.StatusServiceUnavailable {
				wantRetry = "1"
			}
			if got := w.Header().Get("Retry-After"); got != wantRetry {
				t.Errorf("Retry-After=%q, want %q", got, wantRetry)
			}
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, "webhook handler") && strings.Contains(line, `"level":"`+tc.wantLevel+`"`) {
					return
				}
			}
			t.Errorf("missing %s delivery error log: %s", tc.wantLevel, &logs)
		})
	}
}

func TestOnlyWriteLockErrors_NestedJoins(t *testing.T) {
	lockErr := fmt.Errorf("write lock: %w", store.ErrWriteLockUnavailable)
	busyErr := errors.New("SQLITE_BUSY: database is locked")
	storeLockErr := fmt.Errorf("%w: %w", store.ErrWriteLockUnavailable, busyErr)
	wrappedStoreLockErr := fmt.Errorf("record: %w", storeLockErr)
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"lock_only", errors.Join(lockErr, errors.Join(lockErr)), true},
		{"mixed", errors.Join(lockErr, errors.Join(errors.New("store I/O failure"), lockErr)), false},
		{"wrapped_mixed_join", fmt.Errorf("capture push outcomes: %w", errors.Join(fmt.Errorf("commit a: %w", store.ErrWriteLockUnavailable), fmt.Errorf("commit b: %w", io.ErrUnexpectedEOF))), false},
		{"wrapped_lock_only_join", fmt.Errorf("capture push outcomes: %w", errors.Join(fmt.Errorf("commit a: %w", store.ErrWriteLockUnavailable), fmt.Errorf("commit b: %w", store.ErrWriteLockUnavailable))), true},
		{"wrapped_lock", lockErr, true},
		{"single_store_error", fmt.Errorf("write: %w", fmt.Errorf("%w: %w", store.ErrWriteLockUnavailable, context.DeadlineExceeded)), true},
		{"real_shape", storeLockErr, true},
		{"wrapped_real_shape", wrappedStoreLockErr, true},
		{"joined_real_shapes", errors.Join(wrappedStoreLockErr, fmt.Errorf("record another: %w", storeLockErr)), true},
		{"mixed_real_shape_join", errors.Join(wrappedStoreLockErr, fmt.Errorf("commit b: %w", io.ErrUnexpectedEOF)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := onlyWriteLockErrors(tc.err); got != tc.want {
				t.Errorf("onlyWriteLockErrors(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestCapturePushOutcomes_JoinsCommitErrors(t *testing.T) {
	captureErr := errors.New("store I/O failure")
	s := &pushErrorStore{Store: newFakeStore(), captureErr: captureErr}
	h := New(s, testSecret, quietLogger(), WithPushCapture(nil))
	body := []byte(`{
		"ref":"refs/heads/main",
		"repository":{"default_branch":"main","full_name":"acme/app"},
		"commits":[
			{"id":"capture-one", "message":"fix: closes #42", "author":{"username":"alice"}},
			{"id":"capture-two", "message":"fix: closes #43", "author":{"username":"alice"}}
		]
	}`)
	err := h.handlePush(t.Context(), body)
	if !errors.Is(err, captureErr) {
		t.Fatalf("handlePush error=%v, want capture failure", err)
	}
	for _, commit := range []string{"capture-one", "capture-two"} {
		if !strings.Contains(err.Error(), "commit "+logSafeStr(commit)+":") {
			t.Errorf("handlePush error=%v, missing commit %s", err, commit)
		}
	}
}
