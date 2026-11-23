package stats

import (
	"context"
	"sort"
	"sync"
	"time"
)

// rankTTL bounds how long an edit (a merge, a changed credit) can take to show
// in an artist's rank. A new listen refreshes it at once.
const rankTTL = 5 * time.Minute

// rankCache holds every artist's play count, counted the way the top list
// counts (primary credits). Counting reads every listen, too slow to repeat
// on each artist page of a large library.
type rankCache struct {
	mu     sync.Mutex
	at     time.Time
	last   int64           // newest listen id when counted
	counts map[int64]int64 // artist id → plays
	desc   []int64         // the counts, largest first
}

// artistRank is 1 + the number of artists with more plays than id.
func (h *Handler) artistRank(ctx context.Context, id int64) (int64, error) {
	c := &h.ranks
	var last int64
	if err := h.read.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM scrobble`).Scan(&last); err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil || last != c.last || h.now().Sub(c.at) > rankTTL {
		rows, err := h.read.QueryContext(ctx, `SELECT ra.artist_id, COUNT(*) FROM scrobble s
  JOIN recording_artist ra ON ra.recording_id = s.recording_id AND ra.role = 'primary'
  WHERE s.user_id = 1 GROUP BY ra.artist_id`)
		if err != nil {
			return 0, err
		}
		defer rows.Close()
		counts := map[int64]int64{}
		desc := []int64{}
		for rows.Next() {
			var a, n int64
			if err := rows.Scan(&a, &n); err != nil {
				return 0, err
			}
			counts[a] = n
			desc = append(desc, n)
		}
		if err := rows.Err(); err != nil {
			return 0, err
		}
		sort.Slice(desc, func(i, j int) bool { return desc[i] > desc[j] })
		c.counts, c.desc, c.last, c.at = counts, desc, last, h.now()
	}
	mine := c.counts[id]
	return 1 + int64(sort.Search(len(c.desc), func(i int) bool { return c.desc[i] <= mine })), nil
}
