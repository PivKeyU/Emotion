package handlers

import (
	"net/http/httptest"
	"testing"
)

func TestMediaSourceIDFromRequestPrefersQuerySource(t *testing.T) {
	req := httptest.NewRequest("GET", "/Videos/vl-5/stream.mkv?MediaSourceId=media-uuid_&line=", nil)

	if got := mediaSourceIDFromRequest(req); got != "media-uuid" {
		t.Fatalf("mediaSourceIDFromRequest = %q, want media-uuid", got)
	}
}

func TestMediaSourceIDFromRequestAcceptsLowercaseQuery(t *testing.T) {
	req := httptest.NewRequest("GET", "/Videos/vl-5/stream.mkv?mediasourceid=media-uuid_", nil)

	if got := mediaSourceIDFromRequest(req); got != "media-uuid" {
		t.Fatalf("mediaSourceIDFromRequest = %q, want media-uuid", got)
	}
}
