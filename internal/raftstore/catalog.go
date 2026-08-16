package raftstore

// This file contains the catalog-specific replacement checks. Persistence is
// deliberately kept in diskStore; callers publish only a complete image that
// has passed these checks and the durable engine boundary.

func validateCatalogReplacement(previous, next *RangeCatalog) error {
	if previous == nil || next == nil {
		return ErrCatalogCorrupt
	}
	if next.Version() <= previous.Version() {
		return ErrCatalogStale
	}
	for _, descriptor := range next.Descriptors() {
		if err := previous.VerifyGeneration(descriptor); err != nil {
			return err
		}
	}
	previous.mu.RLock()
	defer previous.mu.RUnlock()
	for id, fence := range previous.history {
		if _, current := next.byID[id]; current {
			continue
		}
		if existing, present := next.history[id]; present && !EqualRangeDescriptor(existing, fence) {
			return ErrCatalogStale
		}
		retired := fence.Clone()
		retired.Phase = RangeRetired
		next.history[id] = retired
	}
	return nil
}
