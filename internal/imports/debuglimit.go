package imports

import (
	"container/heap"
	"sort"

	"younithm/internal/importers"
)

// newestN keeps the n most recent listens (by PlayedAt) from a stream, in
// bounded memory. Used only by the debug import limit.
type newestN struct {
	n int
	h listenHeap
}

func (k *newestN) add(l importers.Listen) {
	if len(k.h) < k.n {
		heap.Push(&k.h, l)
		return
	}
	if l.PlayedAt.After(k.h[0].PlayedAt) {
		k.h[0] = l
		heap.Fix(&k.h, 0)
	}
}

// sorted returns the kept listens oldest first.
func (k *newestN) sorted() []importers.Listen {
	out := append([]importers.Listen(nil), k.h...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].PlayedAt.Before(out[j].PlayedAt) })
	return out
}

// listenHeap is a min-heap on PlayedAt (oldest at the root).
type listenHeap []importers.Listen

func (h listenHeap) Len() int           { return len(h) }
func (h listenHeap) Less(i, j int) bool { return h[i].PlayedAt.Before(h[j].PlayedAt) }
func (h listenHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *listenHeap) Push(x any)        { *h = append(*h, x.(importers.Listen)) }
func (h *listenHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
