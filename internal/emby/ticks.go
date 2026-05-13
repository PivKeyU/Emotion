package emby

// TicksPerSecond is the Emby/WMP time unit: 100-nanosecond ticks.
const TicksPerSecond int64 = 10_000_000

// SecondsToTicks converts seconds to Emby ticks.
func SecondsToTicks(seconds int64) int64 {
	return seconds * TicksPerSecond
}
