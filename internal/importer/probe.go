package importer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type MediaProbeInfo struct {
	Metadata  []byte
	Duration  int64
	Size      int64
	Container string
	Bitrate   int64
}

type ffprobeOutput struct {
	Streams  []map[string]any `json:"streams"`
	Chapters []map[string]any `json:"chapters"`
	Format   struct {
		FormatName string `json:"format_name"`
		Duration   any    `json:"duration"`
		Size       any    `json:"size"`
		BitRate    any    `json:"bit_rate"`
	} `json:"format"`
}

func ProbeLocalMedia(ctx context.Context, path string) (*MediaProbeInfo, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("empty media path")
	}
	timeout := 12 * time.Second
	if !isRemoteProbePath(path) {
		timeout = 20 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"-v", "error",
		"-rw_timeout", "10000000",
		"-analyzeduration", "1000000",
		"-probesize", "1048576",
		"-print_format", "json",
		"-show_entries", "format=format_name,duration,size,bit_rate:stream=index,codec_type,codec_name,width,height,bit_rate,avg_frame_rate,r_frame_rate,channels,channel_layout,sample_rate,disposition",
		"-show_streams",
		"-show_format",
		path,
	}
	cmd := exec.CommandContext(probeCtx, "ffprobe", args...)
	out, err := cmd.Output()
	if err != nil {
		if probeCtx.Err() != nil {
			return nil, fmt.Errorf("ffprobe timed out after %s", timeout)
		}
		return nil, err
	}
	var parsed ffprobeOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, err
	}
	if len(parsed.Streams) == 0 && parsed.Format.FormatName == "" {
		return nil, errors.New("ffprobe returned no streams or format")
	}

	info := &MediaProbeInfo{
		Metadata:  out,
		Duration:  parseSeconds(parsed.Format.Duration),
		Size:      parseInt64Any(parsed.Format.Size),
		Container: firstContainer(parsed.Format.FormatName),
		Bitrate:   parseInt64Any(parsed.Format.BitRate),
	}
	return info, nil
}

func isRemoteProbePath(path string) bool {
	path = strings.ToLower(strings.TrimSpace(path))
	return strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://")
}

func firstContainer(formatName string) string {
	formatName = strings.TrimSpace(formatName)
	if formatName == "" {
		return ""
	}
	if i := strings.Index(formatName, ","); i >= 0 {
		return formatName[:i]
	}
	return formatName
}

func parseSeconds(v any) int64 {
	switch x := v.(type) {
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return int64(math.Round(f))
	case float64:
		return int64(math.Round(x))
	case int64:
		return x
	case int:
		return int64(x)
	default:
		return 0
	}
}

func parseInt64Any(v any) int64 {
	switch x := v.(type) {
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return n
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	default:
		return 0
	}
}
