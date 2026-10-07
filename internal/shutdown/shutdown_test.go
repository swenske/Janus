package shutdown

import (
	"slices"
	"testing"
)

func TestRunsOnceInOrder(t *testing.T) {
	var ran []int
	Before(func() { ran = append(ran, 1) })
	Before(func() { ran = append(ran, 2) })
	Run()
	Run()
	if !slices.Equal(ran, []int{1, 2}) {
		t.Errorf("ran %v, want [1 2] once", ran)
	}
}
