package run

import (
	"testing"

	"github.com/rudi-bruchez/SqlGoPace/internal/mssql"
)

// chainSess builds a session blocked by blockedBy (0 = not blocked). Named to avoid
// shadowing the `sess` range variable ServerSampler.Blocking uses.
func chainSess(spid, blockedBy int) mssql.Session {
	return mssql.Session{SPID: spid, BlockingSPID: blockedBy}
}

func TestBlockedBehind(t *testing.T) {
	tests := []struct {
		name     string
		sessions []mssql.Session
		spid     int
		want     int
	}{
		{
			name:     "nothing queued behind",
			sessions: []mssql.Session{chainSess(67, 0), chainSess(79, 67)},
			spid:     79,
			want:     0,
		},
		{
			name: "the PRODDB shape: one victim, sixteen readers",
			sessions: append([]mssql.Session{chainSess(67, 0), chainSess(79, 67), chainSess(119, 0)},
				func() []mssql.Session {
					var out []mssql.Session
					for _, s := range []int{91, 54, 109, 64, 176, 110, 103, 93, 104, 150, 161, 69, 182, 147, 180, 130} {
						out = append(out, chainSess(s, 79))
					}
					return out
				}()...),
			spid: 79,
			want: 16,
		},
		{
			name:     "transitive depth is counted",
			sessions: []mssql.Session{chainSess(1, 0), chainSess(2, 1), chainSess(3, 2), chainSess(4, 3)},
			spid:     1,
			want:     3,
		},
		{
			name:     "unrelated blocked sessions are not counted",
			sessions: []mssql.Session{chainSess(1, 0), chainSess(2, 1), chainSess(8, 0), chainSess(9, 8)},
			spid:     1,
			want:     1,
		},
		{
			name:     "a two-session cycle terminates",
			sessions: []mssql.Session{chainSess(1, 2), chainSess(2, 1)},
			spid:     1,
			want:     1,
		},
		{
			name:     "a three-session cycle terminates",
			sessions: []mssql.Session{chainSess(1, 3), chainSess(2, 1), chainSess(3, 2)},
			spid:     1,
			want:     2,
		},
		{
			name:     "unknown spid has nothing behind it",
			sessions: []mssql.Session{chainSess(1, 0)},
			spid:     999,
			want:     0,
		},
		{
			name:     "empty snapshot",
			sessions: nil,
			spid:     1,
			want:     0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BlockedBehind(tt.sessions, tt.spid); got != tt.want {
				t.Errorf("BlockedBehind(_, %d) = %d, want %d", tt.spid, got, tt.want)
			}
		})
	}
}

func TestDirectVictims(t *testing.T) {
	sessions := []mssql.Session{chainSess(67, 0), chainSess(79, 67), chainSess(91, 79), chainSess(80, 67), chainSess(5, 0)}
	got := DirectVictims(sessions, 67)
	if len(got) != 2 {
		t.Fatalf("DirectVictims returned %d sessions, want 2", len(got))
	}
	if got[0].SPID != 79 || got[1].SPID != 80 {
		t.Errorf("DirectVictims = [%d %d], want [79 80] in snapshot order", got[0].SPID, got[1].SPID)
	}
}

func TestDirectVictimsZeroSPIDMatchesNothing(t *testing.T) {
	sessions := []mssql.Session{chainSess(1, 0), chainSess(2, 0)}
	if got := DirectVictims(sessions, 0); len(got) != 0 {
		t.Errorf("DirectVictims(_, 0) = %d sessions, want 0 — an unknown SPID must never match an idle session", len(got))
	}
}
