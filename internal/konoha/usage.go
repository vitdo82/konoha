package konoha

import (
	"encoding/json"
	"strings"
)

// Agent token/cost accounting (§39, #87). A harness MAY report one model step's
// usage; when it does, the numbers are captured into evidence and the §39 sink.
// When it does not, the contribution is ZERO — never estimated or inferred (the
// ESF rule: a fabricated number is worse than a missing one). Usage is
// observability only: no verdict, route, or terminal-state decision may read it.

// HarnessUsage is the optional usage a harness reports for one agent run. The
// zero value means "not reported" and is never treated as a real measurement.
type HarnessUsage struct {
	TokensIn  int64   `json:"tokensIn,omitempty"`
	TokensOut int64   `json:"tokensOut,omitempty"`
	CostUSD   float64 `json:"costUSD,omitempty"`
}

// IsZero reports whether no usage was reported.
func (u HarnessUsage) IsZero() bool {
	return u.TokensIn == 0 && u.TokensOut == 0 && u.CostUSD == 0
}

// add accumulates another run's usage, ignoring absent/negative components so a
// missing number stays missing rather than reducing the total.
func (u *HarnessUsage) add(o HarnessUsage) {
	if o.TokensIn > 0 {
		u.TokensIn += o.TokensIn
	}
	if o.TokensOut > 0 {
		u.TokensOut += o.TokensOut
	}
	if o.CostUSD > 0 {
		u.CostUSD += o.CostUSD
	}
}

// usageMarker is the byte-exact envelope a harness appends to its stream to
// report one model step's usage: a line starting with KONOHA_USAGE followed by a
// JSON object of tokensIn/tokensOut/costUSD. Absence is normal and yields zero.
// A hostile agent can spoof it, which only pollutes metrics — never a verdict.
const usageMarker = "KONOHA_USAGE"

// parseHarnessUsage best-effort sums every valid KONOHA_USAGE envelope in the
// raw agent stream. A malformed line is ignored; absent usage returns the zero
// value. It never guesses or extrapolates.
func parseHarnessUsage(raw string) HarnessUsage {
	var total HarnessUsage
	for _, line := range strings.Split(raw, "\n") {
		s := strings.TrimSpace(line)
		if !strings.HasPrefix(s, usageMarker) {
			continue
		}
		var u HarnessUsage
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(s, usageMarker))), &u); err != nil {
			continue
		}
		total.add(u)
	}
	return total
}
