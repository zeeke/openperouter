// SPDX-License-Identifier:Apache-2.0

package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/openperouter/openperouter/internal/frrconfig"
)

func TestHandler(t *testing.T) {
	reloadSucceeds := func(_ string) error {
		return nil
	}

	reloadFails := func(_ string) error {
		return errors.New("failed")
	}

	// An AS mismatch the fallback recovered from is indistinguishable from
	// a plain success at the HTTP layer: bgpd was restarted and the reload
	// converged.
	asMismatchRecovered := func(_ string) error {
		return nil
	}

	asMismatchRecoveryFailed := func(_ string) error {
		return errors.New("failed to reload after clearing the BGP instances: frr update reload failed: " +
			"bgpd is running a different AS than the desired configuration")
	}

	tests := []struct {
		name       string
		reloadMock func(string) error
		method     string
		httpStatus int
	}{
		{
			"succeeds",
			reloadSucceeds,
			http.MethodPost,
			200,
		},
		{
			"wrong method",
			reloadSucceeds,
			http.MethodGet,
			http.StatusBadRequest,
		},
		{
			"reload fails",
			reloadFails,
			http.MethodPost,
			http.StatusInternalServerError,
		},
		{
			"AS mismatch recovered by the fallback",
			asMismatchRecovered,
			http.MethodPost,
			200,
		},
		{
			"AS mismatch recovery failed",
			asMismatchRecoveryFailed,
			http.MethodPost,
			http.StatusInternalServerError,
		},
	}

	t.Cleanup(func() {
		updateConfig = frrconfig.Update
	})
	for _, tc := range tests {
		updateConfig = tc.reloadMock
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(tc.method, "/", nil)
			handler := http.HandlerFunc(reloadHandler("/etc/frr/frr.conf"))

			handler.ServeHTTP(w, req)
			res := w.Result()
			if err := res.Body.Close(); err != nil {
				t.Fatalf("Body.Close() failed: %s", err)
			}
			if res.StatusCode != tc.httpStatus {
				t.Fatalf("expecting %d, got %d", res.StatusCode, tc.httpStatus)
			}
		})
	}
}

// TestHandlerSerializesReloads asserts that concurrent reload requests are
// served one at a time: the bgpd restart fallback must not interleave with
// other reloads.
func TestHandlerSerializesReloads(t *testing.T) {
	t.Cleanup(func() {
		updateConfig = frrconfig.Update
	})

	var running sync.Mutex
	inFlight := 0
	overlapped := false
	updateConfig = func(_ string) error {
		running.Lock()
		inFlight++
		if inFlight > 1 {
			overlapped = true
		}
		running.Unlock()

		time.Sleep(10 * time.Millisecond) //nolint:forbidigo // holding the handler busy to detect overlap

		running.Lock()
		inFlight--
		running.Unlock()
		return nil
	}

	handler := http.HandlerFunc(reloadHandler("/etc/frr/frr.conf"))

	const requests = 5
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, "/", nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			res := w.Result()
			if err := res.Body.Close(); err != nil {
				t.Errorf("Body.Close() failed: %s", err)
			}
			if res.StatusCode != http.StatusOK {
				t.Errorf("expecting 200, got %d", res.StatusCode)
			}
		}()
	}
	wg.Wait()

	if overlapped {
		t.Fatal("expecting reload requests to be serialized, got overlapping executions")
	}
}

func TestRemoveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reload.sock")

	if err := removeFile(path); err != nil {
		t.Fatalf("expected a missing socket to be ignored: %v", err)
	}

	err := os.WriteFile(path, []byte{}, 0o600)
	if err != nil {
		t.Fatalf("failed to create temporary file: %v", err)
	}
	if err := removeFile(path); err != nil {
		t.Fatalf("failed to remove unix socket: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected unix socket to be removed, got: %v", err)
	}
}

func TestHealth(t *testing.T) {
	tests := []struct {
		name               string
		check              func() error
		method             string
		expectedHTTPStatus int
	}{
		{
			name:               "healthy",
			check:              func() error { return nil },
			method:             http.MethodGet,
			expectedHTTPStatus: http.StatusOK,
		},
		{
			name:               "unhealthy",
			check:              func() error { return errors.New("watchfrr is not running") },
			method:             http.MethodGet,
			expectedHTTPStatus: http.StatusInternalServerError,
		},
		{
			name:               "wrong method",
			check:              func() error { return nil },
			method:             http.MethodPost,
			expectedHTTPStatus: http.StatusMethodNotAllowed,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, err := http.NewRequest(test.method, "/healthz", nil)
			if err != nil {
				t.Fatalf("failed to create request: %v", err)
			}

			http.HandlerFunc(health(test.check)).ServeHTTP(w, req)
			res := w.Result()
			if err := res.Body.Close(); err != nil {
				t.Fatalf("Body.Close() failed: %s", err)
			}
			if res.StatusCode != test.expectedHTTPStatus {
				t.Fatalf("expecting %d, got %d", test.expectedHTTPStatus, res.StatusCode)
			}
		})
	}
}
