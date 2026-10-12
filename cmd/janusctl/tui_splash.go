package main

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/swenske/Janus/internal/brand"
	"github.com/swenske/Janus/internal/termui"
)

// The splash: Janus's logo while the nodes first answer, its bar
// filling up with their calls and its two faces looking each way in
// turn - for a second at least, any key skipping it.
const (
	splashMin   = time.Second
	splashFrame = 100 * time.Millisecond // the animation's step
	splashLook  = 500 * time.Millisecond // how long a face stays lit
	splashBarW  = 30                     // the progress line's cells
)

// splash is the splash while it shows.
type splash struct {
	start time.Time
}

// splashProgress is what the splash says: the first round's calls over
// every node, how many nodes are done, and the step to show.
type splashProgress struct {
	done, total     int
	finished, nodes int
	step            string
}

func (p splashProgress) frac() float64 {
	if p.total == 0 {
		return 0
	}
	return math.Min(float64(p.done)/float64(p.total), 1)
}

// splashProgress adds up the samplers' first rounds.
func (a *app) splashProgress() splashProgress {
	p := splashProgress{nodes: len(a.samplers)}
	pending := ""
	for _, s := range a.samplers {
		done, total, last, finished := s.firstProgress()
		p.done += done
		p.total += total
		if finished {
			p.finished++
			continue
		}
		if pending == "" {
			pending = s.target.name
			if last != "" {
				pending += " · " + last
			}
		}
	}
	switch {
	case p.nodes > 1 && pending != "":
		p.step = fmt.Sprintf("%d of %d nodes · %s", p.finished, p.nodes, pending)
	case p.nodes > 1:
		p.step = fmt.Sprintf("%d of %d nodes", p.finished, p.nodes)
	case pending != "" && !strings.Contains(pending, " · "):
		p.step = "connecting to " + pending
	case pending != "":
		p.step = pending
	case p.nodes == 1:
		p.step = a.samplers[0].target.name
	}
	return p
}

// splashOver is true once every node finished its first round and the
// splash showed for splashMin.
func (a *app) splashOver() bool {
	p := a.splashProgress()
	return p.finished == p.nodes && a.now().Sub(a.splash.start) >= splashMin
}

// leftLit says which face is lit at now.
func (sp *splash) leftLit(now time.Time) bool {
	return now.Sub(sp.start)/splashLook%2 == 0
}

// renderSplash draws the splash in the middle of f.
func renderSplash(f *termui.Frame, p splashProgress, leftLit bool) {
	frac := p.frac()
	rows := len(brand.Logo)
	// The logo, a blank line, the title, a blank line, the progress, the step.
	x0, y0 := max((f.W-brand.Width)/2, 0), max((f.H-rows-5)/2, 0)
	lit := termui.Style{Bold: true}
	unlit := termui.Style{FG: termui.ColorMuted}
	filled := termui.Style{FG: termui.ColorAccent}
	empty := termui.Style{FG: termui.ColorMeterBG}
	full := int(math.Round(frac * float64(rows))) // bar rows filled, from the bottom
	for i, row := range brand.Logo {
		left, bar, right := brand.Split(row)
		x := x0
		x = splashCells(f, x, y0+i, left, leftLit, lit, unlit)
		if i >= rows-full {
			x = splashCells(f, x, y0+i, bar, true, filled, filled)
		} else {
			x = splashCells(f, x, y0+i, bar, false, filled, empty)
		}
		splashCells(f, x, y0+i, right, !leftLit, lit, unlit)
	}

	y := y0 + rows + 1
	title := "J A N U S"
	f.Text((f.W-len(title))/2, y, title, termui.Style{Bold: true}, 0)

	y += 2
	cells := int(math.Round(frac * splashBarW))
	count := fmt.Sprintf("  %d/%d", p.done, p.total)
	x := max((f.W-splashBarW-len(count))/2, 0)
	x += f.Text(x, y, strings.Repeat("━", cells), filled, 0)
	x += f.Text(x, y, strings.Repeat("─", splashBarW-cells), empty, 0)
	f.Text(x, y, count, unlit, 0)

	y++
	step := termui.Truncate(p.step, f.W-4)
	f.Text(max((f.W-len([]rune(step)))/2, 0), y, step, unlit, 0)
}

// splashCells draws part of a logo row: lit as it is, unlit with its
// full blocks shaded - what reads without colours too.
func splashCells(f *termui.Frame, x, y int, s string, on bool, onSt, offSt termui.Style) int {
	for _, r := range s {
		st := onSt
		if !on {
			st = offSt
			if r == '█' {
				r = '░'
			}
		}
		if r != ' ' {
			f.Set(x, y, r, st)
		}
		x++
	}
	return x
}
