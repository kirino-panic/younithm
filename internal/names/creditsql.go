package names

import "fmt"

// LineSQL is an SQL expression for the credit line of the recording or release
// (kind Recording or Release) whose id is idExpr: each artist under the name
// it is credited as here, else its name set by hand, else its own name, joined
// by the stored join phrases (", " when none), in credit order. It is NULL when
// there are no credits. Every credit line built in SQL uses it, so a line reads
// the same on every page.
func LineSQL(kind, idExpr string) string {
	table, col := "recording_artist", "recording_id"
	if kind == Release {
		table, col = "release_artist", "release_id"
	}
	return fmt.Sprintf(`(SELECT group_concat(part, '') FROM (
  SELECT COALESCE(NULLIF(ca.credited_name, ''), NULLIF(a.display_name, ''), a.name)
    || CASE WHEN ROW_NUMBER() OVER (ORDER BY ca.position DESC, a.id DESC) = 1 THEN '' ELSE COALESCE(NULLIF(ca.join_phrase, ''), ', ') END AS part
  FROM %s ca JOIN artist a ON a.id = ca.artist_id
  WHERE ca.%s = %s ORDER BY ca.position, a.id))`, table, col, idExpr)
}
