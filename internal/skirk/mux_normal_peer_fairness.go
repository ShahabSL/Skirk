package skirk

// rotateNormalPeerLocked gives other ready peers a turn while preserving the
// stream rotation within each peer. The exit caller holds normalMu; using its
// existing bounded order keeps abort/close cleanup in one place.
func (l *muxLane) rotateNormalPeerLocked(selected muxStreamKey) {
	firstSame, sameCount := -1, 0
	needsRotation := false
	for i, key := range l.normalOrder {
		if key.ClientID == selected.ClientID && key.RunID == selected.RunID {
			if firstSame < 0 {
				firstSame = i
			}
			sameCount++
		} else if firstSame >= 0 {
			needsRotation = true
		}
	}
	// Single-peer queues and an already-correct peer suffix need no writes or
	// allocation. This also covers the common one-stream-per-peer case.
	if !needsRotation {
		return
	}
	same := make([]muxStreamKey, 0, sameCount)
	ready := l.normalOrder[:firstSame]
	for _, key := range l.normalOrder[firstSame:] {
		if key.ClientID == selected.ClientID && key.RunID == selected.RunID {
			same = append(same, key)
		} else {
			ready = append(ready, key)
		}
	}
	l.normalOrder = append(ready, same...)
}
