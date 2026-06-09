package handlers

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/PivKeyU/Emotion/internal/db"
	"github.com/PivKeyU/Emotion/internal/server/ctxpkg"
)

func TestResolvePolicyFolderIDsBlockedOnlyKeepsUnblockedLibraries(t *testing.T) {
	allIDs := []int64{1, 2, 3}
	blocked := map[int64]struct{}{2: {}}

	got, storeAll := resolvePolicyFolderIDs(allIDs, db.NullString{}, nil, nil, blocked)

	if storeAll {
		t.Fatalf("storeAll = true, want false")
	}
	if want := []int64{1, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("visible ids = %#v, want %#v", got, want)
	}
}

func TestResolvePolicyFolderIDsExplicitDisableWithoutEnabledFoldersHidesAll(t *testing.T) {
	allIDs := []int64{1, 2, 3}
	enableAll := false

	got, storeAll := resolvePolicyFolderIDs(allIDs, db.NullString{}, &enableAll, nil, nil)

	if storeAll {
		t.Fatalf("storeAll = true, want false")
	}
	if len(got) != 0 {
		t.Fatalf("visible ids = %#v, want empty", got)
	}
}

func TestResolvePolicyFolderIDsEnableAllStoresNullPolicy(t *testing.T) {
	allIDs := []int64{1, 2, 3}
	enableAll := true

	got, storeAll := resolvePolicyFolderIDs(allIDs, db.NullString{Valid: true, String: "[2]"}, &enableAll, nil, nil)

	if !storeAll {
		t.Fatalf("storeAll = false, want true")
	}
	if want := []int64{1, 2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("visible ids = %#v, want %#v", got, want)
	}
}

func TestRouteUserIDAllowsAPIKeyToAddressSpecificUser(t *testing.T) {
	req := httptest.NewRequest("GET", "/emby/Users/42", nil)
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("userId", "42")

	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx)
	ctx = ctxpkg.WithAuth(ctx, 0, "admin-key", true, true)
	req = req.WithContext(ctx)

	if got := routeUserID(req); got != 42 {
		t.Fatalf("routeUserID = %d, want 42", got)
	}
}
