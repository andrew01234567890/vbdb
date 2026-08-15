package failpoint

import (
	"sync"
	"testing"
)

func TestRegistryDisabledByDefaultAndExplicitlyInjectable(t *testing.T) {
	first := New()
	second := New()
	if err := first.Register("before-commit"); err != nil {
		t.Fatal(err)
	}
	if first.Hit("before-commit") {
		t.Fatal("new failpoint was enabled")
	}
	if second.Hit("before-commit") {
		t.Fatal("registries leaked state")
	}
	if err := first.Enable("before-commit"); err != nil {
		t.Fatal(err)
	}
	if !first.Hit("before-commit") || first.Hits("before-commit") != 1 {
		t.Fatal("enabled failpoint did not report and count a hit")
	}
	if second.Enabled("before-commit") {
		t.Fatal("enabling one registry changed another")
	}
	if err := first.Disable("before-commit"); err != nil {
		t.Fatal(err)
	}
	if first.Hit("before-commit") {
		t.Fatal("disabled failpoint reported a hit")
	}
}

func TestRegistryRejectsUnknownAndEmptyNames(t *testing.T) {
	r := New()
	if err := r.Register(""); err == nil {
		t.Fatal("empty name accepted")
	}
	if err := r.Enable("missing"); err == nil {
		t.Fatal("unknown name accepted")
	}
	if err := r.Disable("missing"); err == nil {
		t.Fatal("unknown name accepted")
	}
}

func TestRegistryRaceSafe(t *testing.T) {
	r := New()
	if err := r.Register("io"); err != nil {
		t.Fatal(err)
	}
	if err := r.Enable("io"); err != nil {
		t.Fatal(err)
	}
	const workers = 32
	const hitsPerWorker = 1000
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < hitsPerWorker; j++ {
				_ = r.Enabled("io")
				_ = r.Hit("io")
			}
		}()
	}
	wg.Wait()
	if got, want := r.Hits("io"), uint64(workers*hitsPerWorker); got != want {
		t.Fatalf("hit count = %d, want %d", got, want)
	}
}
