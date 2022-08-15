package sort

type Sort interface {
	Len() int
	Less(i, j int) bool
	Swap(i, j int)
}

func HeapDown(h Sort, i, n int) bool {
	parent := i
	for {
		leftChild := 2*parent + 1
		if leftChild >= n || leftChild < 0 { // leftChild < 0 after int overflow
			break
		}
		lessChild := leftChild
		if rightChild := leftChild + 1; rightChild < n && h.Less(rightChild, leftChild) {
			lessChild = rightChild
		}
		if !h.Less(lessChild, parent) {
			break
		}
		h.Swap(parent, lessChild)
		parent = lessChild
	}
	return parent > i
}

func TopK(h Sort, k int) {
	n := h.Len()
	if k > n {
		k = n
	}
	for i := k/2 - 1; i >= 0; i-- {
		HeapDown(h, i, k)
	}
	if k < n {
		for i := k; i < n; i++ {
			if h.Less(0, i) {
				h.Swap(0, i)
				HeapDown(h, 0, k)
			}
		}
	}
}

func MinHeap(h Sort) {
	n := h.Len()
	for i := n/2 - 1; i >= 0; i-- {
		HeapDown(h, i, n)
	}
}
