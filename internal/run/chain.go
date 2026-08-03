package run

import "github.com/rudi-bruchez/SqlGoPace/internal/mssql"

// DirectVictims returns the sessions blocked directly by ddlSPID, in snapshot order.
// A zero ddlSPID matches nothing, so an unknown session id can never be mistaken for
// one our operation is blocking.
func DirectVictims(sessions []mssql.Session, ddlSPID int) []mssql.Session {
	var out []mssql.Session
	for _, s := range sessions {
		if s.BlockedBy(ddlSPID) {
			out = append(out, s)
		}
	}
	return out
}

// BlockedBehind counts the sessions transitively blocked behind spid, excluding spid
// itself: the fan-out that makes a blocked Sch-M request an amplifier rather than a
// lone waiter.
//
// The walk carries a visited set. A blocking graph assembled row by row from a DMV
// under concurrency is not guaranteed acyclic, and an unguarded walk would not
// terminate.
func BlockedBehind(sessions []mssql.Session, spid int) int {
	if spid == 0 {
		return 0
	}
	behind := make(map[int][]int, len(sessions))
	for _, s := range sessions {
		if s.BlockingSPID != 0 {
			behind[s.BlockingSPID] = append(behind[s.BlockingSPID], s.SPID)
		}
	}
	visited := map[int]bool{spid: true}
	queue, count := []int{spid}, 0
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range behind[cur] {
			if visited[next] {
				continue
			}
			visited[next] = true
			count++
			queue = append(queue, next)
		}
	}
	return count
}
