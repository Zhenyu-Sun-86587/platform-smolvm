package platformapi

import (
	"net/http"
	"testing"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/gateway"
	"sandbox-platform/internal/lifecycle"
)

func TestSessionCookieForManagementOrigin(t *testing.T) {
	for _, tc := range []struct {
		origin, name, path string
		secure             bool
	}{
		{"http://127.0.0.1:8443", gateway.LoopbackSessionCookie, "/v1", false},
		{"http://[::1]:8443", gateway.LoopbackSessionCookie, "/v1", false},
		{"https://127.0.0.1:8443", "__Host-sandbox-session", "/", true},
		{"https://console.example.test", "__Host-sandbox-session", "/", true},
	} {
		cfg := config.Defaults()
		cfg.API.PublicOrigin = tc.origin
		s := Server{Engine: &lifecycle.Engine{Config: cfg}}
		got := s.sessionCookie()
		if got.Name != tc.name || got.Secure != tc.secure || got.Path != tc.path || got.Domain != "" || !got.HttpOnly || got.SameSite != http.SameSiteStrictMode {
			t.Fatalf("%s unexpected cookie attributes: %+v", tc.origin, got)
		}
		req, err := http.NewRequest("GET", "http://app.example.test", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: got.Name, Value: "management-credential"})
		req.AddCookie(&http.Cookie{Name: "user-preference", Value: "visible"})
		gateway.StripRequest(req)
		if req.Header.Get("Cookie") != "user-preference=visible" {
			t.Fatal("unexpected gateway cookies")
		}
	}
}
