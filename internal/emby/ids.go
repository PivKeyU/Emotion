// Package emby contains shared helpers for the Emby-compatible API layer:
// item-id encoding, standard response wrappers, time formatting, etc.
package emby

import (
	"strconv"
	"strings"
)

// ItemID type prefixes matching emya:
//
//	vb = video library
//	vl = video list (movie/series)
//	vs = video season
//	ve = video episode
const (
	ItemIDTypeVideoLibrary = "vb"
	ItemIDTypeVideoList    = "vl"
	ItemIDTypeVideoSeason  = "vs"
	ItemIDTypeVideoEpisode = "ve"
)

// DefaultTime is the Emby "absent time" sentinel.
const DefaultTime = "0001-01-01T00:00:00.0000000Z"

// ItemID combines a type prefix and numeric id, e.g. "vl-42".
func ItemID(kind string, id int64) string {
	return kind + "-" + strconv.FormatInt(id, 10)
}

// ParseItemID extracts (type, id) from "vl-42". Returns false for anything else.
func ParseItemID(value string) (string, int64, bool) {
	parts := strings.SplitN(value, "-", 2)
	if len(parts) != 2 {
		return "", 0, false
	}
	kind := parts[0]
	switch kind {
	case ItemIDTypeVideoLibrary, ItemIDTypeVideoList, ItemIDTypeVideoSeason, ItemIDTypeVideoEpisode:
	default:
		return "", 0, false
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return "", 0, false
	}
	return kind, id, true
}

// ItemResponse is the standard Emby list envelope {Items, TotalRecordCount}.
type ItemResponse struct {
	Items            []any `json:"Items"`
	TotalRecordCount int64 `json:"TotalRecordCount"`
}

// NewItemResponse wraps items with optional total count. When count < 0 the slice length is used.
func NewItemResponse(items []any, count int64) ItemResponse {
	if items == nil {
		items = []any{}
	}
	if count < 0 {
		count = int64(len(items))
	}
	return ItemResponse{Items: items, TotalRecordCount: count}
}
