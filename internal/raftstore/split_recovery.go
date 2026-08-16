package raftstore

// SplitRecoveryManifest is a complete-marker boundary, not a replicated
// coordinator journal. Incomplete staging is never eligible for serving or
// child-image publication after restart.
type SplitRecoveryManifest struct {
	Generation       uint64
	Source           RangeDescriptor
	Barrier          uint64
	SnapshotChecksum uint32
	CatalogVersion   uint64
	Digest           [32]byte
	Complete         bool
}

func (manifest SplitRecoveryManifest) Validate() error {
	if !manifest.Complete {
		return ErrSplitPending
	}
	if manifest.Generation == 0 || manifest.Barrier == 0 || manifest.SnapshotChecksum == 0 || manifest.CatalogVersion == 0 || manifest.Digest == [32]byte{} {
		return ErrSplitChecksum
	}
	if err := manifest.Source.Validate(); err != nil || manifest.Source.Phase == RangeRetired {
		return ErrSplitGeneration
	}
	return nil
}

// RecoverSplitGeneration accepts only a complete, independently validated
// marker. The caller may resend an image from this manifest; it must not infer
// progress from a partial chunk or in-flight Go coordinator state.
func RecoverSplitGeneration(manifest SplitRecoveryManifest) error {
	return manifest.Validate()
}

func ValidateRecoveryCatalog(catalog *RangeCatalog, voters []uint64) error {
	if catalog == nil {
		return ErrCatalogCorrupt
	}
	if catalog.Version() == 0 {
		return ErrCatalogCorrupt
	}
	if err := catalog.ValidateAgainstVoters(voters); err != nil {
		return err
	}
	return nil
}

func IsCompleteSplitGeneration(manifest SplitRecoveryManifest) bool {
	return manifest.Validate() == nil
}
