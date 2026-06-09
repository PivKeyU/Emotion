package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsCompatAnonymousRead(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		method string
		want   bool
	}{
		{name: "prefixed sessions", path: "/emby/Sessions", method: http.MethodGet, want: true},
		{name: "double prefixed sessions", path: "/emby/emby/Sessions", method: http.MethodGet, want: true},
		{name: "root sessions", path: "/Sessions", method: http.MethodGet, want: true},
		{name: "counts", path: "/emby/Items/Counts", method: http.MethodGet, want: true},
		{name: "system info", path: "/emby/System/Info", method: http.MethodGet, want: true},
		{name: "head system info", path: "/emby/System/Info", method: http.MethodHead, want: true},
		{name: "write endpoint", path: "/emby/Sessions/Playing/Stop", method: http.MethodPost, want: false},
		{name: "session child", path: "/emby/Sessions/Playing", method: http.MethodGet, want: false},
		{name: "public info already public", path: "/emby/System/Info/Public", method: http.MethodGet, want: false},
		{name: "items list", path: "/emby/Items", method: http.MethodGet, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if got := isCompatAnonymousRead(req); got != tt.want {
				t.Fatalf("isCompatAnonymousRead(%s %s) = %v, want %v", tt.method, tt.path, got, tt.want)
			}
		})
	}
}
