package termui

// Distribute shares total rows among panels: each gets at least its
// minimum, the rest goes by weight. When the minimums don't fit, the
// last panels are dropped (0 rows) until they do - a small terminal
// shows fewer panels rather than broken ones.
func Distribute(total int, mins, weights []int) []int {
	n := len(mins)
	out := make([]int, n)
	shown := n
	need := 0
	for _, m := range mins {
		need += m
	}
	for shown > 0 && need > total {
		shown--
		need -= mins[shown]
	}
	if shown == 0 {
		return out
	}
	weight := 0
	for i := 0; i < shown; i++ {
		out[i] = mins[i]
		weight += max(weights[i], 0)
	}
	slack := total - need
	if weight == 0 {
		out[0] += slack
		return out
	}
	given := 0
	for i := 0; i < shown; i++ {
		share := slack * max(weights[i], 0) / weight
		out[i] += share
		given += share
	}
	for i := 0; given < slack; i = (i + 1) % shown { // the rounding's remainder, from the first
		if weights[i] > 0 {
			out[i]++
			given++
		}
	}
	return out
}
