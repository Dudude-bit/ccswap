package keychain

import "sync"

// Fake is an in-memory Store for tests.
type Fake struct {
	// OnGet scripts the n-th (1-based) Get of an item; ok=false falls back to the stored value.
	OnGet func(service, account string, n int) ([]byte, bool)

	mu    sync.Mutex
	items map[[2]string][]byte
	gets  map[[2]string]int
}

func NewFake() *Fake {
	return &Fake{items: map[[2]string][]byte{}, gets: map[[2]string]int{}}
}

func (f *Fake) Get(service, account string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := [2]string{service, account}
	f.gets[k]++
	if f.OnGet != nil {
		if b, ok := f.OnGet(service, account, f.gets[k]); ok {
			return append([]byte(nil), b...), nil
		}
	}
	b, ok := f.items[k]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

func (f *Fake) Set(service, account string, secret []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[[2]string{service, account}] = append([]byte(nil), secret...)
	return nil
}

func (f *Fake) Delete(service, account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := [2]string{service, account}
	if _, ok := f.items[k]; !ok {
		return ErrNotFound
	}
	delete(f.items, k)
	return nil
}
