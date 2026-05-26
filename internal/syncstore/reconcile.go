package syncstore

// reconcile.go is the pure, side-effect-free merge (FCIS — functional core). The
// stateful store (store.go) calls into it inside a transaction. It MUST match
// docs/sync/forestnote-sync-protocol.md §5 and pass every vector in
// docs/sync/vectors/ (see vectors_test.go).

// TablePK identifies a materialized row across the three tables.
type TablePK struct {
	Table string
	PK    string
}

// Less reports whether a's LWW key is strictly less than b's, comparing
// (wall_ts, op_seq, site_id) lexicographically (spec §5.1). site_id is compared
// as its ASCII string, which equals ULID numeric order for uppercase Crockford
// (spec §2.1). The op with the GREATER key wins a conflict.
func Less(a, b Op) bool {
	if a.WallTS != b.WallTS {
		return a.WallTS < b.WallTS
	}
	if a.OpSeq != b.OpSeq {
		return a.OpSeq < b.OpSeq
	}
	return a.SiteID < b.SiteID
}

// Merge materializes a set of ops into the winning op per (table, pk), selecting
// the maximum under Less. Because the selection is a total order independent of
// arrival order, the result is identical for any permutation or duplication of
// the same op set (the convergence property, spec §5.2). Each winner's Cols are
// normalized (unknown columns dropped, §3.2).
func Merge(ops []Op) map[TablePK]Op {
	winners := make(map[TablePK]Op)
	for _, op := range ops {
		n := Normalize(op)
		k := TablePK{Table: n.Table, PK: n.PK}
		if cur, ok := winners[k]; !ok || Less(cur, n) {
			winners[k] = n
		}
	}
	return winners
}
