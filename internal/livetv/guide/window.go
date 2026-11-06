// SPDX-License-Identifier: GPL-2.0-only

package guide

import "time"

// MaxGuideDays caps LiveTvOptions.GuideDays (GuideManager.cs:28).
const MaxGuideDays = 14

// DefaultGuideDays applies when GuideDays is null (GuideManager.cs:167-174).
const DefaultGuideDays = 7

// Days returns the guide length in days: GuideDays clamped to 1..14, or 7
// when it is not set (GuideManager.GetGuideDays).
func Days(guideDays *int) int {
	if guideDays == nil {
		return DefaultGuideDays
	}
	return min(max(*guideDays, 1), MaxGuideDays)
}

// RefreshWindow returns the programme window of one refresh: from one hour
// before now to that start plus the guide days (GuideManager.cs:233-234).
func RefreshWindow(now time.Time, guideDays *int) (start, end time.Time) {
	start = now.UTC().Add(-time.Hour)
	return start, start.AddDate(0, 0, Days(guideDays))
}
