// SPDX-License-Identifier: GPL-2.0-only

package sessiondto

import (
	"sort"

	"github.com/deadfin-org/deadfin/internal/dto"
)

// Merge joins this replica's session list with the lists of its peer
// replicas (#663) in the order of session.Manager.Sessions: the most recent
// activity first, then the session ID. A session in two lists keeps its
// first copy.
func Merge(local []dto.SessionInfoDto, peers []dto.SessionInfoDto) []dto.SessionInfoDto {
	if len(peers) == 0 {
		return local
	}
	result := make([]dto.SessionInfoDto, 0, len(local)+len(peers))
	seen := make(map[string]struct{}, len(local)+len(peers))
	for _, list := range [][]dto.SessionInfoDto{local, peers} {
		for _, value := range list {
			if value.ID != nil {
				if _, found := seen[*value.ID]; found {
					continue
				}
				seen[*value.ID] = struct{}{}
			}
			result = append(result, value)
		}
	}
	sort.SliceStable(result, func(left, right int) bool {
		a, b := result[left].LastActivityDate.Time, result[right].LastActivityDate.Time
		if a.Equal(b) {
			return sessionID(result[left]) < sessionID(result[right])
		}
		return a.After(b)
	})
	return result
}

func sessionID(value dto.SessionInfoDto) string {
	if value.ID == nil {
		return ""
	}
	return *value.ID
}
