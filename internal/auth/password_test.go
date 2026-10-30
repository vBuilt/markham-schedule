package auth

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected PHC prefix: %s", h)
	}
	if !CheckPassword(h, "correct horse battery") {
		t.Error("correct password rejected")
	}
	if CheckPassword(h, "wrong") {
		t.Error("wrong password accepted")
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Error("two hashes identical")
	}
	if CheckPassword("garbage", h) || CheckPassword("garbage", "x") || CheckPassword("", "") {
		t.Error("malformed accepted")
	}
}

func TestArgonConcurrencyCapped(t *testing.T) {
	var mu sync.Mutex
	running, peak := 0, 0
	orig := argonKey
	argonKey = func(pw, salt []byte, t, m uint32, p uint8, n uint32) []byte {
		mu.Lock()
		running++
		if running > peak {
			peak = running
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		return make([]byte, n)
	}
	t.Cleanup(func() { argonKey = orig })

	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, _ = HashPassword("correct horse battery")
			} else {
				CheckPassword(h, "correct horse battery")
			}
		}(i)
	}
	wg.Wait()
	if peak > 4 {
		t.Errorf("peak concurrent argon2 runs = %d, want <= 4", peak)
	}
	if peak < 2 {
		t.Errorf("peak = %d; calls did not overlap, test proves nothing", peak)
	}
}
