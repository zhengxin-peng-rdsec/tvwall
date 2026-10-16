package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidDeviceID(t *testing.T) {
	for _, id := range []string{"abc-123", "android_42", "tv.x"} {
		if !validDeviceID(id) {
			t.Errorf("validDeviceID(%q) = false", id)
		}
	}
	for _, id := range []string{"", "../outside", "a/b", strings.Repeat("x", 129)} {
		if validDeviceID(id) {
			t.Errorf("validDeviceID(%q) = true", id)
		}
	}
}

func TestValidDisplayURL(t *testing.T) {
	for _, raw := range []string{"", "http://intranet.local", "https://example.com/path"} {
		if !validDisplayURL(raw) {
			t.Errorf("validDisplayURL(%q) = false", raw)
		}
	}
	for _, raw := range []string{"javascript:alert(1)", "file:///etc/passwd", "//example.com", "not a URL"} {
		if validDisplayURL(raw) {
			t.Errorf("validDisplayURL(%q) = true", raw)
		}
	}
}

func TestRegisterRequiresTokenAfterEnrollment(t *testing.T) {
	s := newStore(t.TempDir())
	s.devices["display-1"] = &Device{ID: "display-1", Token: "secret", IP: "192.0.2.1", RefreshSeconds: defaultRefresh}

	for _, body := range []string{`{"id":"display-1"}`, `{"id":"display-1","token":"wrong"}`} {
		r := httptest.NewRequest(http.MethodPost, "/api/client/register", strings.NewReader(body))
		r.RemoteAddr = "192.0.2.2:1234"
		w := httptest.NewRecorder()
		s.registerClient(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("register with %s status = %d, want %d", body, w.Code, http.StatusUnauthorized)
		}
	}

	r := httptest.NewRequest(http.MethodPost, "/api/client/register", strings.NewReader(`{"id":"display-1","token":"secret"}`))
	r.RemoteAddr = "192.0.2.2:1234"
	w := httptest.NewRecorder()
	s.registerClient(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("valid registration status = %d, want %d", w.Code, http.StatusOK)
	}
}

func TestRegisterInvalidDeviceID(t *testing.T) {
	s := newStore(t.TempDir())
	r := httptest.NewRequest(http.MethodPost, "/api/client/register", strings.NewReader(`{"id":"../../outside"}`))
	w := httptest.NewRecorder()
	s.registerClient(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("register invalid id status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestRegisterPreservesConfiguredNameAndTracksRemoteIP(t *testing.T) {
	s := newStore(t.TempDir())
	s.devices["tv-1"] = &Device{ID: "tv-1", Token: "secret", IP: "192.0.2.10", RefreshSeconds: defaultRefresh}
	r := httptest.NewRequest(http.MethodPost, "/api/client/register", strings.NewReader(`{"id":"tv-1","name":"Pixel"}`))
	r.RemoteAddr = "192.0.2.10:1234"
	w := httptest.NewRecorder()
	s.registerClient(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("initial registration status = %d, want %d", w.Code, http.StatusOK)
	}

	s.mu.Lock()
	d := s.devices["tv-1"]
	d.Name = "一楼大厅"
	token := d.Token
	s.mu.Unlock()

	body := `{"id":"tv-1","name":"Pixel","token":"` + token + `"}`
	r = httptest.NewRequest(http.MethodPost, "/api/client/register", strings.NewReader(body))
	r.RemoteAddr = "192.0.2.20:1234"
	w = httptest.NewRecorder()
	s.registerClient(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat registration status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := s.devices["tv-1"].Name; got != "一楼大厅" {
		t.Fatalf("configured name was overwritten: got %q", got)
	}
	if got := s.devices["tv-1"].IP; got != "192.0.2.20" {
		t.Fatalf("remote IP = %q, want %q", got, "192.0.2.20")
	}
}

func TestRegisterRejectsUnknownClient(t *testing.T) {
	s := newStore(t.TempDir())
	r := httptest.NewRequest(http.MethodPost, "/api/client/register", strings.NewReader(`{"id":"new-client","name":"Pixel"}`))
	r.RemoteAddr = "192.0.2.30:1234"
	w := httptest.NewRecorder()
	s.registerClient(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unknown registration status = %d, want %d", w.Code, http.StatusForbidden)
	}
	if len(s.devices) != 0 {
		t.Fatalf("unknown client was added to store: %v", s.devices)
	}
}

func TestRegisterClaimsFixedIPEntry(t *testing.T) {
	s := newStore(t.TempDir())
	s.devices["pending-tv"] = &Device{ID: "pending-tv", Name: "一楼大厅", IP: "192.0.2.40", Token: "pending-token", RefreshSeconds: defaultRefresh}
	r := httptest.NewRequest(http.MethodPost, "/api/client/register", strings.NewReader(`{"id":"android-real-id","name":"Pixel","token":"stale-token"}`))
	r.RemoteAddr = "192.0.2.40:1234"
	w := httptest.NewRecorder()
	s.registerClient(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("fixed-IP claim status = %d, want %d", w.Code, http.StatusOK)
	}
	if _, ok := s.devices["pending-tv"]; ok {
		t.Fatal("pending fixed-IP entry was not replaced")
	}
	d, ok := s.devices["android-real-id"]
	if !ok {
		t.Fatal("real client ID was not registered")
	}
	if d.Name != "一楼大厅" {
		t.Fatalf("claimed name = %q, want %q", d.Name, "一楼大厅")
	}
	if d.Token == "" || d.Token == "pending-token" {
		t.Fatalf("claimed client token was not rotated: %q", d.Token)
	}
}

func TestDeviceListHasStableEnrollmentOrder(t *testing.T) {
	s := newStore(t.TempDir())
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.devices["tv-late"] = &Device{ID: "tv-late", CreatedAt: base.Add(2 * time.Minute)}
	s.devices["tv-first"] = &Device{ID: "tv-first", CreatedAt: base}
	s.devices["tv-middle"] = &Device{ID: "tv-middle", CreatedAt: base.Add(time.Minute)}

	w := httptest.NewRecorder()
	s.deviceList(w)
	if w.Code != http.StatusOK {
		t.Fatalf("device list status = %d, want %d", w.Code, http.StatusOK)
	}
	var out struct {
		Devices []struct {
			ID string `json:"id"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode device list: %v", err)
	}
	got := make([]string, 0, len(out.Devices))
	for _, d := range out.Devices {
		got = append(got, d.ID)
	}
	want := []string{"tv-first", "tv-middle", "tv-late"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("device order = %v, want %v", got, want)
	}
}
