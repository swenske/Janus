package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/swenske/Janus/internal/brand"
	"github.com/swenske/Janus/internal/termui"
)

func TestSplashFrame(t *testing.T) {
	f := termui.NewFrame(80, 24)
	renderSplash(f, splashProgress{done: 12, total: 18, nodes: 1, step: "lgslbpub01 · haproxy"}, true)
	golden(t, "tui-splash-80x24.txt", f)

	// 12/18 fills the bar's 9 lowest rows of 14, in the accent; above,
	// its blocks are shaded. The left face is lit, the right one shaded.
	x0, y0 := (80-brand.Width)/2, (24-len(brand.Logo)-5)/2
	for _, c := range []struct {
		name  string
		x, y  int
		r     rune
		style termui.Style
	}{
		{"bar, top", x0 + 11, y0 + 1, '░', termui.Style{FG: termui.ColorMeterBG}},
		{"bar, 5th row", x0 + 11, y0 + 4, '░', termui.Style{FG: termui.ColorMeterBG}},
		{"bar, 6th row", x0 + 11, y0 + 5, '█', termui.Style{FG: termui.ColorAccent}},
		{"bar, bottom", x0 + 11, y0 + 13, '▀', termui.Style{FG: termui.ColorAccent}},
		{"left face", x0 + 3, y0 + 6, '█', termui.Style{Bold: true}},
		{"right face", x0 + 20, y0 + 6, '░', termui.Style{FG: termui.ColorMuted}},
	} {
		if got := f.At(c.x, c.y); got.R != c.r || got.S != c.style {
			t.Errorf("%s: %q %+v, want %q %+v", c.name, got.R, got.S, c.r, c.style)
		}
	}

	// Half a second later, the other face.
	f = termui.NewFrame(80, 24)
	renderSplash(f, splashProgress{done: 12, total: 18, nodes: 1}, false)
	if l, r := f.At(x0+3, y0+6), f.At(x0+20, y0+6); l.R != '░' || r.R != '█' {
		t.Errorf("right face lit: left %q, right %q", l.R, r.R)
	}
	sp := &splash{start: tuiNow}
	if !sp.leftLit(tuiNow.Add(400*time.Millisecond)) || sp.leftLit(tuiNow.Add(600*time.Millisecond)) || !sp.leftLit(tuiNow.Add(time.Second)) {
		t.Error("the faces take turns every half second, the left one first")
	}
}

func TestSplashProgressAndEnd(t *testing.T) {
	a := testApp(t)
	answered := testSampler(t, a, "lgslbpub01")
	a.samplers = []*sampler{answered}
	a.splash = &splash{start: tuiNow}

	p := a.splashProgress()
	if p.finished != 1 || p.done != p.total || p.total != firstCalls(false) || p.step != "lgslbpub01" {
		t.Errorf("one node answered: %+v", p)
	}
	a.now = func() time.Time { return tuiNow.Add(500 * time.Millisecond) }
	if a.splashOver() {
		t.Error("the splash shows for a second at least")
	}
	a.now = func() time.Time { return tuiNow.Add(splashMin) }
	if !a.splashOver() {
		t.Error("everything answered and a second gone: the splash is over")
	}

	// A node still on its first round keeps it up, however long.
	waiting := newSampler(tuiTarget{name: "lgslbpub02", dial: func() (*grpc.ClientConn, error) { return nil, errors.New("not dialled") }}, a.wake, &a.interval, &a.paused)
	a.samplers = []*sampler{waiting}
	if p := a.splashProgress(); p.step != "connecting to lgslbpub02" || p.done != 0 {
		t.Errorf("before any answer: %+v", p)
	}
	waiting.firstTotal.Store(int64(firstCalls(false)))
	waiting.firstDone.Store(4)
	waiting.firstLast.Store("memory")
	a.samplers = []*sampler{answered, waiting}
	p = a.splashProgress()
	if p.step != "1 of 2 nodes · lgslbpub02 · memory" || p.done != firstCalls(false)+4 || p.total != 2*firstCalls(false) {
		t.Errorf("fleet, one node waiting: %+v", p)
	}
	a.now = func() time.Time { return tuiNow.Add(time.Minute) }
	if a.splashOver() {
		t.Error("a node still on its first round keeps the splash up")
	}
}

func TestSplashKeys(t *testing.T) {
	a := testApp(t)
	a.splash = &splash{start: tuiNow}
	if a.key("x") || a.splash != nil {
		t.Error("any key skips the splash, without leaving")
	}
	a.splash = &splash{start: tuiNow}
	if !a.key("q") {
		t.Error("q leaves from the splash too")
	}
}

// TestFirstProgressFailedDial: a node that can't be dialled ends its
// first round at once - the splash doesn't wait for it.
func TestFirstProgressFailedDial(t *testing.T) {
	a := testApp(t)
	s := newSampler(tuiTarget{name: "gone", dial: func() (*grpc.ClientConn, error) { return nil, errors.New("no route") }}, a.wake, &a.interval, &a.paused)
	s.round(context.Background())
	if done, total, _, finished := s.firstProgress(); !finished || done != total || total != firstCalls(false) {
		t.Errorf("after a failed dial: %d/%d finished %v", done, total, finished)
	}
}

// TestFirstCalls keeps firstCalls true to the calls a first round
// makes: each one ends (refused here) and says so.
func TestFirstCalls(t *testing.T) {
	conn, err := grpc.NewClient("passthrough:///127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var n atomic.Int64
	count := func(string) { n.Add(1) }
	fetchSample(ctx, conn, true, count)
	fetchSlow(ctx, conn, count)
	if int(n.Load()) != firstCalls(true) {
		t.Errorf("a full first round made %d calls, firstCalls says %d", n.Load(), firstCalls(true))
	}
	n.Store(0)
	fetchSample(ctx, conn, false, count)
	fetchSlow(ctx, conn, count)
	if int(n.Load()) != firstCalls(false) {
		t.Errorf("a first round made %d calls, firstCalls says %d", n.Load(), firstCalls(false))
	}
}
